package phantomfile

import (
	"fmt"
	"log"
	"sync/atomic"

	"golang.org/x/net/context"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

var _ fs.HandleFlusher = (*handle)(nil)
var _ fs.HandleReader = (*handle)(nil)
var _ fs.HandleWriter = (*handle)(nil)
var _ fs.HandleReleaser = (*handle)(nil)

type handle struct {
	pf       *PhantomFile
	of       *openFile
	am       AccessMode
	released uint32 // only access via atomic
}

func newHandle(pf *PhantomFile, am AccessMode) *handle {
	log.Printf("handle: newHandle %q as %s", pf.of.du, am)
	return &handle{
		pf: pf,
		of: pf.of,
		am: am,
	}
}

func (h *handle) isReleased() bool {
	return atomic.LoadUint32(&h.released) != 0
}

func (h *handle) release() bool {
	return atomic.CompareAndSwapUint32(&h.released, 0, 1)
}

func (h *handle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	log.Printf("handle: flushing %q", h.of.du)
	if h.isReleased() {
		log.Printf("Attempt to flush released handle for %q, failing", h.pf.du)
		return fuse.ESTALE
	}
	if !h.am.isWriteable() {
		// This is quite common, apparently
		return nil
	}
	return h.of.flush(ctx)
}

func (h *handle) Read(ctx context.Context, req *fuse.ReadRequest, res *fuse.ReadResponse) error {
	log.Printf("handle: reading %q", h.of.du)
	if h.isReleased() {
		log.Printf("Attempt to read from released handle for %q, failing", h.pf.du)
		return fuse.ESTALE
	}
	if !h.am.isReadable() {
		return fuse.EPERM
	}
	return h.of.read(ctx, req, res)
}

func (h *handle) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	log.Printf("handle: writing %q", h.of.du)
	if h.isReleased() {
		log.Printf("Attempt to write to released handle for %q, failing", h.pf.du)
		return fuse.ESTALE
	}
	if !h.am.isWriteable() {
		return fuse.EPERM
	}
	return h.of.write(ctx, req, resp)
}

func (h *handle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	log.Printf("handle: releasing %q", h.of.du)
	if !h.release() {
		log.Printf("Attempt to release already released handle for %q, failing", h.pf.du)
		return fuse.ESTALE
	}
	var flushErr error
	if h.am.isWriteable() {
		flushErr = h.of.flush(ctx)
	}
	err := h.pf.release(ctx)
	if flushErr != nil {
		return flushErr
	}
	return err
}

func (h *handle) String() string {
	return fmt.Sprintf("handle{%s,am=%s}", h.of.du, h.am)
}
