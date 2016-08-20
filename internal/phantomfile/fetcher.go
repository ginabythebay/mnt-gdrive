package phantomfile

import (
	"log"
	"os"
	"sync"

	"golang.org/x/net/context"
)

type downloader interface {
	Download(context.Context, *os.File) error
	String() string
}

// fetcher manages downloading of contents from a gdrive file into an
// os file, up to one time
type fetcher struct {
	ctx    context.Context
	cancel context.CancelFunc
	dl     downloader

	mu   sync.Mutex
	file *os.File
	done bool
	err  error
}

// newFetcher returns a new fetcher.
func newFetcher(ctx context.Context, dl downloader, fm FetchMode, file *os.File) *fetcher {
	ctx, cancel := context.WithCancel(ctx)
	f := &fetcher{
		ctx:    ctx,
		cancel: cancel,
		dl:     dl,
		file:   file,
	}
	switch fm {
	case NoFetch:
		f.done = true
	case ProactiveFetch:
		go f.fetch()
	default:
		// nothing more to do
	}
	return f
}

// Fetch does the actual fetching, unless it has already been attempted.  If it has
// already been done, the error (possibly nil) from the first attempt will be returned.
func (f *fetcher) fetch() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.done {
		defer func() {
			f.done = true
		}()

		if f.err = f.ctx.Err(); f.err == nil {
			log.Printf("fetching content for %q...", f.dl)
			if f.err = f.dl.Download(f.ctx, f.file); f.err != nil {
				log.Printf("Failed to download content for %q/%q: %v", f.dl, f.file.Name(), f.err)
			}
		}
	}
	if f.err == context.Canceled {
		return nil
	}
	return f.err
}

// Abort terminates any existing fetching process, returning after the termination is
// complete.  Subsequent calls to Fetch will be immediately succeed.
func (f *fetcher) abort() {
	f.cancel()
	f.fetch()
}
