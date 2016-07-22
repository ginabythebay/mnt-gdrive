package gdrive

import (
	"fmt"
	"log"

	"bazil.org/fuse"
	"golang.org/x/net/context"
	"google.golang.org/api/drive/v3"
)

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
