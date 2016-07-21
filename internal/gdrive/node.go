package gdrive

import (
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"

	"golang.org/x/net/context"

	"bazil.org/fuse"
)

const pageSize = 1000

const fileFields = "id, name, ownedByMe, createdTime, modifiedTime, size, version, parents, fileExtension, mimeType, trashed"
const fileGroupFields = "nextPageToken, files(" + fileFields + ")"

// TODO(gina) future out how to unexport this after this package handles changes

// IncludeFile decides if we want to to include the gdrive file in our system
func IncludeFile(f *drive.File) bool {
	// TODO(gina) make the OwnedByMe check configurable
	return !strings.Contains(f.Name, "/") && f.OwnedByMe
}

// Node represents raw metadata about a file or directory that came from google drive.
// Mostly a simple data-holder
type Node struct {
	// should never change
	ID string

	Name      string
	Ctime     time.Time
	Mtime     time.Time
	Size      uint64
	Version   int64
	ParentIDs []string
	Trashed   bool

	// We use these to determine if it is a folder
	FileExtension string
	MimeType      string
}

// TODO(gina) we probably should not be returning fuse errors,
// but should translate them in the callers

// TODO(gina) I think we can stop exporting NewNode after I pulled
// everything that calls it out into this package.

// NewNode create a Node, based on an id, and on a google file.
func NewNode(id string, f *drive.File) (*Node, error) {
	var ctime time.Time
	ctime, err := time.Parse(time.RFC3339, f.CreatedTime)
	if err != nil {
		log.Printf("Error parsing ctime %#v of node %#v: %s\n", f.CreatedTime, id, err)
		return nil, fuse.ENODATA
	}

	var mtime time.Time
	mtime, err = time.Parse(time.RFC3339, f.ModifiedTime)
	if err != nil {
		log.Printf("Error parsing mtime %#v of node %#v: %s\n", f.ModifiedTime, id, err)
		return nil, fuse.ENODATA
	}

	return &Node{id,
		f.Name,
		ctime,
		mtime,
		uint64(f.Size),
		f.Version,
		f.Parents,
		f.Trashed,
		f.FileExtension,
		f.MimeType}, nil
}

// FetchNode looks up a Node by id and either returns it or an error.
func FetchNode(service *drive.Service, id string) (n *Node, err error) {
	f, err := service.Files.Get(id).
		Fields(fileFields).
		Do()
	if err != nil {
		log.Print("Unable to fetch node info.", err)
		return nil, fuse.ENODATA
	}
	if !IncludeFile(f) || f.Trashed {
		return nil, fuse.ENODATA
	}

	return NewNode(id, f)
}

// FetchChildren returns a slice of children, or an error.
func FetchChildren(ctx context.Context, service *drive.Service, id string) (children []*Node, err error) {
	handler := func(r *drive.FileList) error {
		for _, f := range r.Files {
			if !IncludeFile(f) {
				continue
			}
			// if there was an error in NewNode, we logged it and we will just skip it here
			if g, _ := NewNode(f.Id, f); err == nil {
				children = append(children, g)
			}
		}
		return nil
	}

	// TODO(gina) we need to exclude items that are not in 'my drive', to match what
	// we are doing in changes.  we could do it in the query below maybe, or filter it in
	// the handler above, where we filter on name

	err = service.Files.List().
		PageSize(pageSize).
		Fields(fileGroupFields).
		Q(fmt.Sprintf("'%s' in parents and trashed = false", id)).
		Pages(ctx, handler)
	if err != nil {
		log.Print("Unable to retrieve files.", err)
		return nil, fuse.ENODATA
	}
	return children, nil
}

// Dir returns true if this google file appears to be a directory.
func (n *Node) Dir() bool {
	// see https://developers.google.com/drive/v3/web/folder
	if n.MimeType == "application/vnd.google-apps.folder" && n.FileExtension == "" {
		return true
	}
	return false
}
