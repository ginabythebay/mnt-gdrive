package main

import (
	"log"
	"os"
	"sync"

	"github.com/ginabythebay/mnt-gdrive/internal/gdrive"

	"golang.org/x/net/context"
)

// Fetcher manages downloading of contents from a gdrive file into an os file, up to one time
type Fetcher struct {
	ctx    context.Context
	cancel context.CancelFunc
	dl     gdrive.DriveLike
	id     string

	mu   sync.Mutex
	file *os.File
	done bool
	err  error
}

// NewFetcher returns a new fetcher.
func NewFetcher(ctx context.Context, dl gdrive.DriveLike, id string, file *os.File) *Fetcher {
	ctx, cancel := context.WithCancel(ctx)
	return &Fetcher{
		ctx:    ctx,
		cancel: cancel,
		dl:     dl,
		id:     id,
		file:   file,
	}
}

// Fetch does the actual fetching, unless it has already been attempted.  If it has
// already been done, the error (possibly nil) from the first attempt will be returned.
func (f *Fetcher) Fetch() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.done {
		defer func() {
			f.done = true
		}()

		if f.err = f.ctx.Err(); f.err != nil {
			return f.err
		}

		log.Printf("fetching content for %q...", f.id)
		if f.err = f.dl.Download(f.ctx, f.id, f.file); f.err != nil {
			log.Printf("Failed to download content for %q/%q: %v", f.id, f.file.Name(), f.err)
		}
	}
	return f.err
}

// Abort terminates any existing fetching process, returning after the termination is
// complete.  Subsequent calls to Fetch will be immediately fail.
func (f *Fetcher) Abort() {
	f.cancel()
	f.Fetch()
}
