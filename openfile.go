package main

import (
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

var _ fs.HandleReader = (*openFile)(nil)
var _ fs.HandleWriter = (*openFile)(nil)
var _ fs.HandleReleaser = (*openFile)(nil)

type openFile struct {
	n  *node
	am accessMode
	fm fetchMode

	fetcher *Fetcher
	tmpFile *os.File

	dirtyMu sync.Mutex
	dirty   bool
}

func newOpenFile(n *node, am accessMode, fm fetchMode) (fr *openFile, err error) {
	tmpFile, err := ioutil.TempFile("", n.name)
	if err != nil {
		log.Printf("Error creating temp file for %s: %v", n.name, err)
		return nil, fuse.EIO
	}

	fr = &openFile{
		n:       n,
		am:      am,
		fm:      fm,
		fetcher: NewFetcher(context.Background(), n.gd, n.id, tmpFile),
		tmpFile: tmpFile}
	if fm == proactiveFetch {
		go fr.fetcher.Fetch()
	}
	return fr, nil
}

func (o *openFile) Read(ctx context.Context, req *fuse.ReadRequest, res *fuse.ReadResponse) error {
	if !o.am.isReadable() {
		return fuse.EPERM
	}
	if o.fm == proactiveFetch || o.fm == fetchAsNeeded {
		if err := o.fetcher.Fetch(); err != nil {
			return fuse.EIO
		}
	}

	if o.tmpFile == nil {
		return fuse.EIO
	}
	b := make([]byte, req.Size)
	n, err := o.tmpFile.ReadAt(b, req.Offset)
	if err != nil && err != io.EOF {
		log.Printf("Error reading from temp file: %v", err)
		return fuse.EIO
	}
	res.Data = b[:n]
	return nil
}

func (o *openFile) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if !o.am.isWriteable() {
		return fuse.EPERM
	}
	if o.fm == proactiveFetch || o.fm == fetchAsNeeded {
		if err := o.fetcher.Fetch(); err != nil {
			log.Printf("Write fetcher error for %q: %v", o.n.name, err)
			return fuse.EIO
		}
	}

	o.markDirty(true)

	var err error
	resp.Size, err = o.tmpFile.WriteAt(req.Data, req.Offset)
	if err != nil {
		log.Printf("Error writing %q for write to %q: %v", o.n.name, req.Offset, err)
		return fuse.EIO
	}

	return nil
}

func (o *openFile) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	log.Printf("openFile: releasing %q", o.n.id)
	o.fetcher.Abort()

	var err error
	if o.isDirty() {
		err = o.n.gd.Upload(ctx, o.n.id, o.tmpFile)
	}

	name := o.tmpFile.Name()
	if closeErr := o.tmpFile.Close(); closeErr != nil {
		log.Printf("Error closing %s: %v", name, closeErr)
		return closeErr
	}
	if rmErr := os.Remove(name); rmErr != nil {
		log.Printf("Error removing %s: %v", name, rmErr)
		return rmErr
	}

	if err != nil {
		log.Printf("Upload of %q/%q failed: %v", o.n.id, o.n.name, err)
	}
	return err
}

func (o *openFile) isDirty() bool {
	o.dirtyMu.Lock()
	defer o.dirtyMu.Unlock()
	return o.dirty
}

func (o *openFile) markDirty(dirty bool) {
	o.dirtyMu.Lock()
	o.dirty = dirty
	o.dirtyMu.Unlock()
}

const (
	readOnly  accessMode = syscall.O_RDONLY
	writeOnly accessMode = syscall.O_WRONLY
	readWrite accessMode = syscall.O_RDWR
)

type accessMode uint32

func (am accessMode) isReadable() bool {
	return am == readOnly || am == readWrite
}

func (am accessMode) isWriteable() bool {
	return am == writeOnly || am == readWrite
}

type fetchMode uint32

const (
	proactiveFetch fetchMode = iota
	fetchAsNeeded
	noFetch
)
