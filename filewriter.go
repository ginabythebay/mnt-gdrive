package main

import (
	"io/ioutil"
	"log"
	"os"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

var _ fs.HandleWriter = (*fileWriter)(nil)
var _ fs.HandleReleaser = (*fileWriter)(nil)

type fileWriter struct {
	n *node

	mu      sync.Mutex
	tmpFile *os.File
}

func newFilewWriter(n *node) (*fileWriter, error) {
	tmpFile, err := ioutil.TempFile("", n.name)
	if err != nil {
		log.Printf("Error creating temp file for %s: %v", n.name, err)
		return nil, fuse.EIO
	}
	return &fileWriter{n: n, tmpFile: tmpFile}, nil
}

func (w *fileWriter) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.tmpFile.WriteAt(req.Data, req.Offset); err != nil {
		log.Printf("Error writing %q for write to %q: %v", w.n.name, req.Offset, err)
		return fuse.EIO
	}

	return nil
}

func (w *fileWriter) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	w.mu.Lock()
	defer func() {
		w.mu.Unlock()

		err := w.tmpFile.Close()
		name := w.tmpFile.Name()
		if err != nil {
			log.Printf("Error closing %s: %v", name, err)
		}
		os.Remove(name)
	}()
	err := w.n.gd.Upload(ctx, w.n.id, w.tmpFile)

	return err
}
