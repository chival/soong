// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"compress/flate"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"android/soong/third_party/zip"
)

// Block size used during parallel compression of a single file.
const parallelBlockSize = 1 * 1024 * 1024 // 1MB

// Minimum file size to use parallel compression. It requires more
// flate.Writer allocations, since we can't change the dictionary
// during Reset
const minParallelFileSize = parallelBlockSize * 6

// Size of the ZIP compression window (32KB)
const windowSize = 32 * 1024

type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error {
	return nil
}

type fileArg struct {
	relativeRoot, file string
}

type fileArgs []fileArg

func (l *fileArgs) String() string {
	return `""`
}

func (l *fileArgs) Set(s string) error {
	if *relativeRoot == "" {
		return fmt.Errorf("must pass -C before -f")
	}

	*l = append(*l, fileArg{filepath.Clean(*relativeRoot), s})
	return nil
}

func (l *fileArgs) Get() interface{} {
	return l
}

var (
	out          = flag.String("o", "", "file to write zip file to")
	manifest     = flag.String("m", "", "input jar manifest file name")
	directories  = flag.Bool("d", false, "include directories in zip")
	relativeRoot = flag.String("C", "", "path to use as relative root of files in next -f or -l argument")
	parallelJobs = flag.Int("j", runtime.NumCPU(), "number of parallel threads to use")
	compLevel    = flag.Int("L", 5, "deflate compression level (0-9)")
	listFiles    fileArgs
	files        fileArgs

	cpuProfile = flag.String("cpuprofile", "", "write cpu profile to file")
	traceFile  = flag.String("trace", "", "write trace to file")
)

func init() {
	flag.Var(&listFiles, "l", "file containing list of .class files")
	flag.Var(&files, "f", "file to include in zip")
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: soong_zip -o zipfile [-m manifest] -C dir [-f|-l file]...\n")
	flag.PrintDefaults()
	os.Exit(2)
}

type zipWriter struct {
	time        time.Time
	createdDirs map[string]bool
	directories bool

	errors   chan error
	writeOps chan chan *zipEntry

	rateLimit *RateLimit

	compressorPool sync.Pool
	compLevel      int
}

type zipEntry struct {
	fh *zip.FileHeader

	// List of delayed io.Reader
	futureReaders chan chan io.Reader
}

func main() {
	flag.Parse()

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *traceFile != "" {
		f, err := os.Create(*traceFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		defer f.Close()
		err = trace.Start(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		defer trace.Stop()
	}

	if *out == "" {
		fmt.Fprintf(os.Stderr, "error: -o is required\n")
		usage()
	}

	w := &zipWriter{
		time:        time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC),
		createdDirs: make(map[string]bool),
		directories: *directories,
		compLevel:   *compLevel,
	}

	err := w.write(*out, listFiles, *manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func (z *zipWriter) write(out string, listFiles fileArgs, manifest string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}

	defer f.Close()
	defer func() {
		if err != nil {
			os.Remove(out)
		}
	}()

	z.errors = make(chan error)
	defer close(z.errors)

	// This channel size can be essentially unlimited -- it's used as a fifo
	// queue decouple the CPU and IO loads. Directories don't require any
	// compression time, but still cost some IO. Similar with small files that
	// can be very fast to compress. Some files that are more difficult to
	// compress won't take a corresponding longer time writing out.
	//
	// The optimum size here depends on your CPU and IO characteristics, and
	// the the layout of your zip file. 1000 was chosen mostly at random as
	// something that worked reasonably well for a test file.
	//
	// The RateLimit object will put the upper bounds on the number of
	// parallel compressions and outstanding buffers.
	z.writeOps = make(chan chan *zipEntry, 1000)
	z.rateLimit = NewRateLimit(*parallelJobs, 0)
	defer z.rateLimit.Stop()

	go func() {
		var err error
		defer close(z.writeOps)

		for _, listFile := range listFiles {
			err = z.writeListFile(listFile)
			if err != nil {
				z.errors <- err
				return
			}
		}

		for _, file := range files {
			err = z.writeRelFile(file.relativeRoot, file.file)
			if err != nil {
				z.errors <- err
				return
			}
		}

		if manifest != "" {
			err = z.writeFile("META-INF/MANIFEST.MF", manifest)
			if err != nil {
				z.errors <- err
				return
			}
		}
	}()

	zipw := zip.NewWriter(f)

	var currentWriteOpChan chan *zipEntry
	var currentWriter io.WriteCloser
	var currentReaders chan chan io.Reader
	var currentReader chan io.Reader
	var done bool

	for !done {
		var writeOpsChan chan chan *zipEntry
		var writeOpChan chan *zipEntry
		var readersChan chan chan io.Reader

		if currentReader != nil {
			// Only read and process errors
		} else if currentReaders != nil {
			readersChan = currentReaders
		} else if currentWriteOpChan != nil {
			writeOpChan = currentWriteOpChan
		} else {
			writeOpsChan = z.writeOps
		}

		select {
		case writeOp, ok := <-writeOpsChan:
			if !ok {
				done = true
			}

			currentWriteOpChan = writeOp

		case op := <-writeOpChan:
			currentWriteOpChan = nil

			if op.fh.Method == zip.Deflate {
				currentWriter, err = zipw.CreateCompressedHeader(op.fh)
			} else {
				var zw io.Writer
				zw, err = zipw.CreateHeader(op.fh)
				currentWriter = nopCloser{zw}
			}
			if err != nil {
				return err
			}

			currentReaders = op.futureReaders
			if op.futureReaders == nil {
				currentWriter.Close()
				currentWriter = nil
			}

		case futureReader, ok := <-readersChan:
			if !ok {
				// Done with reading
				currentWriter.Close()
				currentWriter = nil
				currentReaders = nil
			}

			currentReader = futureReader

		case reader := <-currentReader:
			var count int64
			count, err = io.Copy(currentWriter, reader)
			if err != nil {
				return err
			}
			z.rateLimit.Release(int(count))

			currentReader = nil

		case err = <-z.errors:
			return err
		}
	}

	// One last chance to catch an error
	select {
	case err = <-z.errors:
		return err
	default:
		zipw.Close()
		return nil
	}
}

func (z *zipWriter) writeListFile(listFile fileArg) error {
	list, err := ioutil.ReadFile(listFile.file)
	if err != nil {
		return err
	}

	files := strings.Split(string(list), "\n")

	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		err = z.writeRelFile(listFile.relativeRoot, file)
		if err != nil {
			return err
		}
	}

	return nil
}

func (z *zipWriter) writeRelFile(root, file string) error {
	file = filepath.Clean(file)

	rel, err := filepath.Rel(root, file)
	if err != nil {
		return err
	}

	err = z.writeFile(rel, file)
	if err != nil {
		return err
	}

	return nil
}

func (z *zipWriter) writeFile(rel, file string) error {
	var fileSize int64

	if s, err := os.Lstat(file); err != nil {
		return err
	} else if s.IsDir() {
		if z.directories {
			return z.writeDirectory(rel)
		}
		return nil
	} else if s.Mode()&os.ModeSymlink != 0 {
		return z.writeSymlink(rel, file)
	} else if !s.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file, directory, or symlink", file)
	} else {
		fileSize = s.Size()
	}

	if z.directories {
		dir, _ := filepath.Split(rel)
		err := z.writeDirectory(dir)
		if err != nil {
			return err
		}
	}

	compressChan := make(chan *zipEntry, 1)
	z.writeOps <- compressChan

	// Pre-fill a zipEntry, it will be sent in the compressChan once
	// we're sure about the Method and CRC.
	ze := &zipEntry{
		fh: &zip.FileHeader{
			Name:   rel,
			Method: zip.Deflate,

			UncompressedSize64: uint64(fileSize),
		},
	}
	ze.fh.SetModTime(z.time)

	r, err := os.Open(file)
	if err != nil {
		return err
	}

	exec := z.rateLimit.RequestExecution()

	if fileSize >= minParallelFileSize {
		wg := new(sync.WaitGroup)

		// Allocate enough buffer to hold all readers. We'll limit
		// this based on actual buffer sizes in RateLimit.
		ze.futureReaders = make(chan chan io.Reader, (fileSize/parallelBlockSize)+1)

		// Calculate the CRC in the background, since reading the entire
		// file could take a while.
		//
		// We could split this up into chuncks as well, but it's faster
		// than the compression. Due to the Go Zip API, we also need to
		// know the result before we can begin writing the compressed
		// data out to the zipfile.
		wg.Add(1)
		go z.crcFile(r, ze, exec, compressChan, wg)

		for start := int64(0); start < fileSize; start += parallelBlockSize {
			sr := io.NewSectionReader(r, start, parallelBlockSize)
			resultChan := make(chan io.Reader, 1)
			ze.futureReaders <- resultChan

			exec := z.rateLimit.RequestExecution()

			last := !(start+parallelBlockSize < fileSize)
			var dict []byte
			if start >= windowSize {
				dict, err = ioutil.ReadAll(io.NewSectionReader(r, start-windowSize, windowSize))
			}

			wg.Add(1)
			go z.compressPartialFile(sr, dict, last, exec, resultChan, wg)
		}

		close(ze.futureReaders)

		// Close the file handle after all readers are done
		go func(wg *sync.WaitGroup, f *os.File) {
			wg.Wait()
			f.Close()
		}(wg, r)
	} else {
		go z.compressWholeFile(rel, r, exec, compressChan)
	}

	return nil
}

func (z *zipWriter) crcFile(r io.Reader, ze *zipEntry, exec Execution, resultChan chan *zipEntry, wg *sync.WaitGroup) {
	defer wg.Done()
	defer exec.Finish(0)

	crc := crc32.NewIEEE()
	_, err := io.Copy(crc, r)
	if err != nil {
		z.errors <- err
		return
	}

	ze.fh.CRC32 = crc.Sum32()
	resultChan <- ze
	close(resultChan)
}

func (z *zipWriter) compressPartialFile(r io.Reader, dict []byte, last bool, exec Execution, resultChan chan io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()

	result, err := z.compressBlock(r, dict, last)
	if err != nil {
		z.errors <- err
		return
	}

	exec.Finish(result.Len())
	resultChan <- result
}

func (z *zipWriter) compressBlock(r io.Reader, dict []byte, last bool) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	var fw *flate.Writer
	var err error
	if len(dict) > 0 {
		// There's no way to Reset a Writer with a new dictionary, so
		// don't use the Pool
		fw, err = flate.NewWriterDict(buf, z.compLevel, dict)
	} else {
		var ok bool
		if fw, ok = z.compressorPool.Get().(*flate.Writer); ok {
			fw.Reset(buf)
		} else {
			fw, err = flate.NewWriter(buf, z.compLevel)
		}
		defer z.compressorPool.Put(fw)
	}
	if err != nil {
		return nil, err
	}

	_, err = io.Copy(fw, r)
	if err != nil {
		return nil, err
	}
	if last {
		fw.Close()
	} else {
		fw.Flush()
	}

	return buf, nil
}

func (z *zipWriter) compressWholeFile(rel string, r *os.File, exec Execution, compressChan chan *zipEntry) {
	var bufSize int

	defer r.Close()

	fileHeader := &zip.FileHeader{
		Name:   rel,
		Method: zip.Deflate,
	}
	fileHeader.SetModTime(z.time)

	crc := crc32.NewIEEE()
	count, err := io.Copy(crc, r)
	if err != nil {
		z.errors <- err
		return
	}

	fileHeader.CRC32 = crc.Sum32()
	fileHeader.UncompressedSize64 = uint64(count)

	_, err = r.Seek(0, 0)
	if err != nil {
		z.errors <- err
		return
	}

	compressed, err := z.compressBlock(r, nil, true)

	ze := &zipEntry{
		fh:            fileHeader,
		futureReaders: make(chan chan io.Reader, 1),
	}
	futureReader := make(chan io.Reader, 1)
	ze.futureReaders <- futureReader
	close(ze.futureReaders)

	if uint64(compressed.Len()) < ze.fh.UncompressedSize64 {
		futureReader <- compressed
		bufSize = compressed.Len()
	} else {
		_, err = r.Seek(0, 0)
		if err != nil {
			z.errors <- err
			return
		}

		buf, err := ioutil.ReadAll(r)
		if err != nil {
			z.errors <- err
			return
		}

		ze.fh.Method = zip.Store
		futureReader <- bytes.NewReader(buf)
		bufSize = int(ze.fh.UncompressedSize64)
	}
	exec.Finish(bufSize)
	close(futureReader)

	compressChan <- ze
	close(compressChan)
}

func (z *zipWriter) writeDirectory(dir string) error {
	if dir != "" && !strings.HasSuffix(dir, "/") {
		dir = dir + "/"
	}

	for dir != "" && dir != "./" && !z.createdDirs[dir] {
		z.createdDirs[dir] = true

		dirHeader := &zip.FileHeader{
			Name: dir,
		}
		dirHeader.SetMode(0700 | os.ModeDir)
		dirHeader.SetModTime(z.time)

		ze := make(chan *zipEntry, 1)
		ze <- &zipEntry{
			fh: dirHeader,
		}
		close(ze)
		z.writeOps <- ze

		dir, _ = filepath.Split(dir)
	}

	return nil
}

func (z *zipWriter) writeSymlink(rel, file string) error {
	if z.directories {
		dir, _ := filepath.Split(rel)
		if err := z.writeDirectory(dir); err != nil {
			return err
		}
	}

	fileHeader := &zip.FileHeader{
		Name: rel,
	}
	fileHeader.SetModTime(z.time)
	fileHeader.SetMode(0700 | os.ModeSymlink)

	dest, err := os.Readlink(file)
	if err != nil {
		return err
	}

	ze := make(chan *zipEntry, 1)
	futureReaders := make(chan chan io.Reader, 1)
	futureReader := make(chan io.Reader, 1)
	futureReaders <- futureReader
	close(futureReaders)
	futureReader <- bytes.NewBufferString(dest)
	close(futureReader)

	// We didn't ask permission to execute, since this should be very short
	// but we still need to increment the outstanding buffer sizes, since
	// the read will decrement the buffer size.
	z.rateLimit.Release(-len(dest))

	ze <- &zipEntry{
		fh:            fileHeader,
		futureReaders: futureReaders,
	}
	close(ze)
	z.writeOps <- ze

	return nil
}
