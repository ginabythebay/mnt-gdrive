package gdrive

import (
	"fmt"
	"io"
	"log"
	"os"

	"google.golang.org/api/drive/v3"

	"bazil.org/fuse"
	"golang.org/x/net/context"
)

// FetchNode looks up a Node by id and either returns it or an error.
func (gd *Gdrive) FetchNode(id string) (n *Node, err error) {
	f, err := gd.svc.Files.Get(id).
		Fields(fileFields).
		Do()
	if err != nil {
		log.Print("Unable to fetch node info.", err)
		return nil, fuse.ENODATA
	}
	n, err = newNode(id, f)
	if err != nil {
		return nil, err
	}
	if !n.IncludeNode() {
		return nil, fuse.ENODATA
	}
	return n, nil
}

// CreateNode creates a child file or directory
func (gd *Gdrive) CreateNode(parentID string, name string, dir bool) (n *Node, err error) {
	var mimeType string
	if dir {
		mimeType = "application/vnd.google-apps.folder"
	}
	f, err := gd.svc.Files.Create(&drive.File{
		Name:     name,
		Parents:  []string{parentID},
		MimeType: mimeType}).
		Fields(fileFields).
		Do()
	if err != nil {
		log.Printf("Unable to create node %q: %v", name, err)
		return nil, fuse.EIO
	}
	n, err = newNode(f.Id, f)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// FetchChildren returns a slice of children, or an error.
func (gd *Gdrive) FetchChildren(ctx context.Context, id string) (children []*Node, err error) {
	handler := func(r *drive.FileList) error {
		for _, f := range r.Files {
			c, err := newNode(f.Id, f)
			// if there was an error in newNode, we logged it and we
			// will just skip it here
			if err != nil || !c.IncludeNode() {
				continue
			}
			children = append(children, c)
		}
		return nil
	}

	// TODO(gina) we need to exclude items that are not in 'my drive', to match what
	// we are doing in changes.  we could do it in the query below maybe, or filter it in
	// the handler above, where we filter on name

	err = gd.svc.Files.List().
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

// Download downloads a files contents to an already open file, f.
func (gd *Gdrive) Download(id string, f *os.File) error {
	resp, err := gd.svc.Files.Get(id).Download()
	if err != nil {
		log.Printf("Unable to download %s: %v", id, err)
		return err
	}
	defer resp.Body.Close()

	b := make([]byte, 1024*8)
	for {
		len, err := resp.Body.Read(b)
		if len > 0 {
			f.Write(b[0:len])
		}
		if err == io.EOF {
			break
		} else if err != nil {
			log.Printf("Error fetching bytes for %s: %v", id, err)
			return err
		}
		// else loop around again
	}
	return nil
}
