package main

import (
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

var _ fs.HandleReader = (*fileReader)(nil)
var _ fs.HandleReleaser = (*fileReader)(nil)

type fileReader struct {
	n              *node
	ctx            context.Context
	cancelDownload context.CancelFunc
	init           sync.Once
	tmpFileMu      sync.Mutex
	tmpFile        *os.File
}

func newFileReader(n *node) (fr *fileReader) {
	ctx, cancelDownload := context.WithCancel(context.Background())
	fr = &fileReader{n: n, ctx: ctx, cancelDownload: cancelDownload}
	go fr.init.Do(fr.fetch)
	return fr
}

func (r *fileReader) fetch() {
	tmpFile, err := ioutil.TempFile("", r.n.name)
	if err != nil {
		log.Printf("Error creating temp file for %s: %v", r.n.name, err)
		return
	}
	defer func() {
		r.tmpFileMu.Lock()
		if r.tmpFile == nil {
			tmpFile.Close()
		}
		r.tmpFileMu.Unlock()
	}()

	log.Printf("fetching content for %q...", r.n.name)
	err = r.n.gd.Download(r.ctx, r.n.id, tmpFile)
	if err != nil {
		log.Printf("Failed to download content for %q/%q: %v", r.n.id, r.n.name, err)
		return
	}
	r.tmpFileMu.Lock()
	r.tmpFile = tmpFile
	r.tmpFileMu.Unlock()
}

func (r *fileReader) Read(ctx context.Context, req *fuse.ReadRequest, res *fuse.ReadResponse) error {
	r.init.Do(r.fetch)

	r.tmpFileMu.Lock()
	defer r.tmpFileMu.Unlock()

	if r.tmpFile == nil {
		return fuse.EIO
	}
	b := make([]byte, req.Size)
	n, err := r.tmpFile.ReadAt(b, req.Offset)
	if err != nil && err != io.EOF {
		log.Printf("Error reading from temp file: %v", err)
		return fuse.EIO
	}
	res.Data = b[:n]
	return nil
}

func (r *fileReader) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	// race condition.  This can run while fetch is running.  Need to think about some kind of lock/cancellation thingy
	r.cancelDownload()
	r.tmpFileMu.Lock()
	defer r.tmpFileMu.Unlock()

	log.Printf("FileReader: releasing %q", r.n.id)
	if r.tmpFile == nil {
		return nil
	}
	err := r.tmpFile.Close()
	name := r.tmpFile.Name()
	if err != nil {
		log.Printf("Error closing %s: %v", name, err)
	}
	os.Remove(name)
	return err
}
