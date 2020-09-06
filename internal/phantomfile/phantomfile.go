package phantomfile

import (
	"fmt"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"

	"golang.org/x/net/context"
)

// DownloaderUploader is something we know how to download and upload
type DownloaderUploader interface {
	Download(context.Context, *os.File) error
	Upload(context.Context, *os.File) error
	ID() string
	Name() string
	String() string
}

// PhantomFile handles file-related requests for gdrive files that
// sometimes have a local presence on the file system (e.g. while
// open) and sometimes don't.
type PhantomFile struct {
	du          DownloaderUploader
	mu          sync.Mutex
	handleCount uint32
	of          *openFile
}

func NewPhantomFile(du DownloaderUploader) *PhantomFile {
	return &PhantomFile{du: du}
}

func (pf *PhantomFile) Open(am AccessMode, fm FetchMode) (*handle, error) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.of == nil {
		of, err := newOpenFile(pf.du, fm)
		if err != nil {
			return nil, err
		}
		pf.of = of
	}

	pf.handleCount++
	return newHandle(pf, am), nil
}

func (pf *PhantomFile) StatIfLocal() (size int64, modTime time.Time, ok bool) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	defer func() {
		log.Printf("StatIfLocal: of nil=%t, size=%d, modTime=%q, ok=%t",
			pf.of == nil, size, modTime, ok)
	}()
	if pf.of == nil {
		return size, modTime, false
	}
	fi, err := pf.of.stat()
	if err != nil {
		log.Printf("StatIfLocal for %q failed: %v", pf.of, err)
		return size, modTime, false
	}

	return fi.Size(), fi.ModTime(), true
}

func (pf *PhantomFile) Truncate(ctx context.Context, size int64) error {
	var fm FetchMode
	if size == 0 {
		fm = NoFetch
	} else {
		fm = ProactiveFetch
	}
	h, err := pf.Open(WriteOnly, fm)
	if err != nil {
		return err
	}
	defer h.Release(ctx, &fuse.ReleaseRequest{})
	if err = h.of.truncate(size); err != nil {
		return err
	}

	return h.Flush(ctx, &fuse.FlushRequest{})
}

func (pf *PhantomFile) release(ctx context.Context) error {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	pf.handleCount--
	if pf.handleCount > 0 {
		return nil
	}
	err := pf.of.release(ctx)
	pf.of = nil
	return err
}

const (
	ProactiveFetch FetchMode = iota
	FetchAsNeeded
	NoFetch
)

// FetchMode indicates whether we should fetch file contents at all,
// and when to start fetching them.
type FetchMode uint32

func (fm FetchMode) String() string {
	switch fm {
	case ProactiveFetch:
		return "ProactiveFetch"
	case FetchAsNeeded:
		return "FetchAsNeeded"
	case NoFetch:
		return "NoFetch"
	default:
		return fmt.Sprintf("Unknown mode %d", fm)
	}
}

const (
	ReadOnly  AccessMode = syscall.O_RDONLY
	WriteOnly AccessMode = syscall.O_WRONLY
	ReadWrite AccessMode = syscall.O_RDWR
)

// AccessMode indicates whether we are opening a file for reading, for writing, or for both.
type AccessMode uint32

func (am AccessMode) String() string {
	switch am {
	case ReadOnly:
		return "ReadOnly"
	case WriteOnly:
		return "WriteOnly"
	case ReadWrite:
		return "ReadWrite"
	default:
		return fmt.Sprintf("Unknown mode %d", am)
	}
}

func (am AccessMode) isReadable() bool {
	return am == ReadOnly || am == ReadWrite
}

func (am AccessMode) isWriteable() bool {
	return am == WriteOnly || am == ReadWrite
}
