// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package vfstest

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/cockroachdb/pebble/vfs"
)

// WithOpenFileTracking wraps a FS, returning an FS that will monitor open
// files. The second return value is a func that when invoked prints the stacks
// that opened the currently open files. If no files are open, the func writes
// nothing.
func WithOpenFileTracking(inner vfs.FS) (vfs.FS, func(io.Writer)) {
	wrappedFS := &openFilesFS{
		inner: inner,
		files: make(map[*openFile]struct{}),
	}
	return wrappedFS, wrappedFS.dumpStacks
}

type openFilesFS struct {
	inner vfs.FS
	mu    sync.Mutex
	files map[*openFile]struct{}
}

var _ vfs.FS = (*openFilesFS)(nil)

func (fs *openFilesFS) dumpStacks(w io.Writer) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.files) == 0 {
		return
	}
	fmt.Fprintf(w, "%d open files:\n", len(fs.files))
	for f := range fs.files {
		f.dumpStack(w)
		fmt.Fprintln(w)
	}
}

func (fs *openFilesFS) Create(name string) (vfs.File, error) {
	f, err := fs.inner.Create(name)
	return fs.wrapOpenFile(f), err
}

func (fs *openFilesFS) Link(oldname, newname string) error {
	return fs.inner.Link(oldname, newname)
}

func (fs *openFilesFS) Open(name string, opts ...vfs.OpenOption) (vfs.File, error) {
	f, err := fs.inner.Open(name, opts...)
	return fs.wrapOpenFile(f), err
}

func (fs *openFilesFS) OpenReadWrite(name string, opts ...vfs.OpenOption) (vfs.File, error) {
	f, err := fs.inner.OpenReadWrite(name, opts...)
	return fs.wrapOpenFile(f), err
}

func (fs *openFilesFS) OpenDir(name string) (vfs.File, error) {
	f, err := fs.inner.OpenDir(name)
	return fs.wrapOpenFile(f), err
}

func (fs *openFilesFS) Remove(name string) error {
	return fs.inner.Remove(name)
}

func (fs *openFilesFS) RemoveAll(name string) error {
	return fs.inner.RemoveAll(name)
}

func (fs *openFilesFS) Rename(oldname, newname string) error {
	return fs.inner.Rename(oldname, newname)
}

func (fs *openFilesFS) ReuseForWrite(oldname, newname string) (vfs.File, error) {
	f, err := fs.inner.ReuseForWrite(oldname, newname)
	return fs.wrapOpenFile(f), err
}

func (fs *openFilesFS) MkdirAll(dir string, perm os.FileMode) error {
	return fs.inner.MkdirAll(dir, perm)
}

func (fs *openFilesFS) Lock(name string) (io.Closer, error) {
	return fs.inner.Lock(name)
}

func (fs *openFilesFS) List(dir string) ([]string, error) {
	return fs.inner.List(dir)
}

func (fs *openFilesFS) Stat(name string) (os.FileInfo, error) {
	return fs.inner.Stat(name)
}

func (fs *openFilesFS) PathBase(path string) string {
	return fs.inner.PathBase(path)
}

func (fs *openFilesFS) PathJoin(elem ...string) string {
	return fs.inner.PathJoin(elem...)
}

func (fs *openFilesFS) PathDir(path string) string {
	return fs.inner.PathDir(path)
}

func (fs *openFilesFS) GetDiskUsage(path string) (vfs.DiskUsage, error) {
	return fs.inner.GetDiskUsage(path)
}

func (fs *openFilesFS) wrapOpenFile(f vfs.File) vfs.File {
	if f == nil {
		return f
	}
	of := &openFile{File: f, parent: fs}
	of.n = runtime.Callers(2, of.pcs[:])
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.files[of] = struct{}{}
	return of
}

type openFile struct {
	vfs.File
	parent *openFilesFS
	pcs    [20]uintptr
	n      int
}

func (f *openFile) dumpStack(w io.Writer) {
	frames := runtime.CallersFrames(f.pcs[:f.n])
	for {
		frame, more := frames.Next()
		fmt.Fprintf(w, "%s\n %s:%d\n", frame.Function, frame.File, frame.Line)
		if !more {
			break
		}
	}
}

func (f *openFile) Close() error {
	err := f.File.Close()
	f.parent.mu.Lock()
	defer f.parent.mu.Unlock()
	delete(f.parent.files, f)
	return err
}
