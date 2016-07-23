package gdrive

import (
	"log"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"

	"bazil.org/fuse"
)

const pageSize = 1000

const fileFields = "id, name, ownedByMe, createdTime, modifiedTime, size, version, parents, fileExtension, mimeType, trashed"
const fileGroupFields = "nextPageToken, files(" + fileFields + ")"

const changeFields = "changes/*, kind, newStartPageToken, nextPageToken"

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
	OwnedByMe bool
	Trashed   bool

	// We use these to determine if it is a folder
	FileExtension string
	MimeType      string
}

// TODO(gina) we probably should not be returning fuse errors,
// but should translate them in the callers

func newNode(id string, f *drive.File) (*Node, error) {
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
		f.OwnedByMe,
		f.Trashed,
		f.FileExtension,
		f.MimeType}, nil
}

// Dir returns true if this google file appears to be a directory.
func (n *Node) Dir() bool {
	// see https://developers.google.com/drive/v3/web/folder
	if n.MimeType == "application/vnd.google-apps.folder" && n.FileExtension == "" {
		return true
	}
	return false
}

// IncludeNode decides if we want to to include the node in our system
func (n *Node) IncludeNode() bool {
	// TODO(gina) make the OwnedByMe check configurable
	return !n.Trashed && !strings.Contains(n.Name, "/") && n.OwnedByMe
}
