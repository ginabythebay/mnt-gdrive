package fakedrive

import (
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"log"
	"os"

	"bazil.org/fuse"
	"golang.org/x/net/context"

	"github.com/ginabythebay/mnt-gdrive/internal/gdrive"
)

func pseudoUUID() (uuid string) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		fmt.Println("Error: ", err)
		return
	}

	uuid = fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])

	return
}

// MakeDir returns a new gdrive directory, suitable for testing.
func MakeDir(id string, name string, parentID string) *gdrive.Node {
	parents := []string{}
	if parentID != "" {
		parents = []string{parentID}
	}
	return &gdrive.Node{ID: id, Name: name, ParentIDs: parents, MimeType: "application/vnd.google-apps.folder"}
}

func contentForTextFile(id string) []byte {
	return []byte(fmt.Sprintf("content for %s", id))
}

// MakeTextFile returns a new gdrive text file, suitable for testing.
func MakeTextFile(id string, name string, parentID string) *gdrive.Node {
	parents := []string{parentID}
	n := &gdrive.Node{
		ID:            id,
		Name:          name,
		ParentIDs:     parents,
		MimeType:      "text/plain",
		FileExtension: ".txt"}
	n.Size = uint64(len(contentForTextFile(id)))
	return n
}

// Drive represents a fake drive, for integration testing
type Drive struct {
	allNodes []*gdrive.Node
	// Maps from id to the content.  If no entry, we fall back to
	// calling contentForTextFile
	contentMap map[string][]byte
}

// NewDrive returns a new fake drive.
func NewDrive(allNodes []*gdrive.Node) *Drive {
	return &Drive{allNodes, map[string][]byte{}}
}

func (fake *Drive) newID() (id string) {
	idSet := map[string]bool{}
	for _, n := range fake.allNodes {
		idSet[n.ID] = true
	}
	for {
		candidate := pseudoUUID()
		if _, found := idSet[candidate]; !found {
			return candidate
		}
	}
}

// FetchNode looks up a node by id in our in-memory data structure.
func (fake *Drive) FetchNode(id string) (n *gdrive.Node, err error) {
	for _, n := range fake.allNodes {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, fuse.ENOENT
}

// CreateNode creates a fake node and puts it into our in memory data structure.
func (fake *Drive) CreateNode(parentID string, name string, dir bool) (n *gdrive.Node, err error) {
	id := fake.newID()
	if dir {
		n = MakeDir(id, name, parentID)
	} else {
		n = MakeTextFile(id, name, parentID)
	}
	return n, nil
}

// FetchChildren looks up the children in memory for an id.
func (fake *Drive) FetchChildren(ctx context.Context, id string) (children []*gdrive.Node, err error) {
	if _, err := fake.FetchNode(id); err != nil {
		return nil, err
	}
	for _, n := range fake.allNodes {
		for _, p := range n.ParentIDs {
			if p == id {
				children = append(children, n)
			}
		}
	}

	return children, nil
}

// Download copies content from our in memory node into a file.
func (fake *Drive) Download(ctx context.Context, id string, f *os.File) error {
	content, ok := fake.contentMap[id]
	if !ok {
		content = contentForTextFile(id)
	}
	fmt.Printf(":: fake downloading %q for %q\n", content, id)
	f.Write(content)
	return nil
}

// Upload copies content for our in memory node from a file.
func (fake *Drive) Upload(ctx context.Context, id string, f *os.File) error {
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	content, err := ioutil.ReadAll(f)
	if err != nil {
		return err
	}
	fmt.Printf(":: fake uploading %q to %q\n", content, id)
	fake.contentMap[id] = content
	return nil
}

// Rename moves and/or renames a node.
func (fake *Drive) Rename(ctx context.Context, id string, newName string, oldParentID string, newParentID string) (n *gdrive.Node, err error) {
	n, err = fake.FetchNode(id)
	if err != nil {
		return nil, err
	}
	if newName != "" {
		n.Name = newName
	}
	if oldParentID != "" {
		if err = reparent(n, oldParentID, newParentID); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// Trash removes the node entry if it exists and the content entry, if it exists.
func (fake *Drive) Trash(ctx context.Context, id string) error {
	for i, node := range fake.allNodes {
		if node.ID == id {
			fake.allNodes = append(fake.allNodes[:i], fake.allNodes[i+1:]...)
			break
		}
	}
	delete(fake.contentMap, id)
	return nil
}

// ProcessChanges doesn't work yet.
func (fake *Drive) ProcessChanges(changeHandler func(*gdrive.Change, *gdrive.ChangeStats)) (gdrive.ChangeStats, error) {
	log.Fatal("implement me")
	return gdrive.ChangeStats{}, fuse.EIO
}

func reparent(n *gdrive.Node, oldParentID string, newParentID string) error {
	for i, id := range n.ParentIDs {
		if id == oldParentID {
			n.ParentIDs[i] = newParentID
			return nil
		}
	}
	return fmt.Errorf("id %q is not a parent of %q", oldParentID, n.ID)
}
