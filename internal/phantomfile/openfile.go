package phantomfile

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"bazil.org/fuse"
	"golang.org/x/net/context"
)

type openFile struct {
	du DownloaderUploader

	fetcher *fetcher
	tmpFile *os.File

	dirtyMu sync.Mutex
	dirty   bool
}

func newOpenFile(du DownloaderUploader, fm FetchMode) (fr *openFile, err error) {
	tmpFile, err := ioutil.TempFile("", fmt.Sprintf("mntgd-%s-%s-", du.ID(), du.Name()))
	if err != nil {
		log.Printf("Error creating temp file for %s: %v", du, err)
		return nil, fuse.EIO
	}

	fr = &openFile{
		du:      du,
		fetcher: newFetcher(context.Background(), du, fm, tmpFile),
		tmpFile: tmpFile}
	log.Printf("openFile: creating %q with fetchMode of %s", du, fm)

	return fr, nil
}

func (o *openFile) read(ctx context.Context, req *fuse.ReadRequest, res *fuse.ReadResponse) error {
	if err := o.fetcher.fetch(); err != nil {
		return fuse.EIO
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

func (o *openFile) write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if err := o.fetcher.fetch(); err != nil {
		log.Printf("Write fetcher error for %q: %v", o.du, err)
		return fuse.EIO
	}

	var err error
	resp.Size, err = o.tmpFile.WriteAt(req.Data, req.Offset)
	if err != nil {
		log.Printf("Error writing %q for write to %q: %v", o.du, req.Offset, err)
		return fuse.EIO
	}

	o.markDirty()

	return nil
}

func (o *openFile) release(ctx context.Context) error {
	log.Printf("openFile: releasing %q", o.du)
	o.fetcher.abort()

	var err error
	name := o.tmpFile.Name()
	if closeErr := o.tmpFile.Close(); closeErr != nil {
		log.Printf("Error closing %s: %v", name, closeErr)
		return closeErr
	}
	if rmErr := os.Remove(name); rmErr != nil {
		log.Printf("Error removing %s: %v", name, rmErr)
		return rmErr
	}
	return err
}

func (o *openFile) truncate(size int64) error {
	err := o.tmpFile.Truncate(size)
	o.markDirty()
	return err
}

func (o *openFile) flush(ctx context.Context) error {
	o.dirtyMu.Lock()
	defer o.dirtyMu.Unlock()
	if !o.dirty {
		log.Printf("openFile: declining to flush %q because it is not dirty", o.du)
		return nil
	}
	err := o.du.Upload(ctx, o.tmpFile)
	if err == nil {
		o.dirty = false
	}
	log.Printf("openFile: flush of %q returning %v", o.du, err)
	return err
}

func (o *openFile) markDirty() {
	o.dirtyMu.Lock()
	o.dirty = true
	o.dirtyMu.Unlock()
}
