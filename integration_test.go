package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"testing"

	"github.com/ginabythebay/mnt-gdrive/internal/gdrive"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"golang.org/x/net/context"
)

func init() {
	fstestutil.DebugByDefault()
}

func allNodes() []*gdrive.Node {
	return []*gdrive.Node{
		makeDir("root", "", ""),
		makeDir("dir_one_id", "dir one", "root"),
		makeDir("dir_two_id", "dir two", "root"),
		makeTextFile("file_one_id", "file one", "root"),
		makeTextFile("file_two_id", "file two", "dir_two_id"),
	}
}

func makeDir(id string, name string, parentID string) *gdrive.Node {
	parents := []string{}
	if parentID != "" {
		parents = []string{parentID}
	}
	return &gdrive.Node{ID: id, Name: name, ParentIDs: parents, MimeType: "application/vnd.google-apps.folder"}
}

func contentForTextFile(n *gdrive.Node) []byte {
	return []byte(fmt.Sprintf("content for %s", n.Name))
}

func makeTextFile(id string, name string, parentId string) *gdrive.Node {
	parents := []string{parentId}
	n := &gdrive.Node{
		ID:            id,
		Name:          name,
		ParentIDs:     parents,
		MimeType:      "text/plain",
		FileExtension: ".txt"}
	n.Size = uint64(len(contentForTextFile(n)))
	return n
}

type fakeDrive struct {
	allNodes []*gdrive.Node
}

func (fake *fakeDrive) FetchNode(id string) (n *gdrive.Node, err error) {
	for _, n := range fake.allNodes {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, fuse.ENOENT
}

func (fake *fakeDrive) FetchChildren(ctx context.Context, id string) (children []*gdrive.Node, err error) {
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

func (fake *fakeDrive) Download(id string, f *os.File) error {
	n, err := fake.FetchNode(id)
	if err != nil {
		return err
	}
	f.Write(contentForTextFile(n))
	return nil
}

func (fake *fakeDrive) ProcessChanges(pageToken *string, changeHandler func(*gdrive.Change) uint32) (uint32, error) {
	log.Fatal("implement me")
	return 0, fuse.EIO
}

func mount(mnt *fstestutil.Mount) fs.FS {
	return wrapper{&fakeDrive{allNodes()}, mnt.Server}
}

func neverErr(fi os.FileInfo) error {
	return nil
}

func TestScenario(t *testing.T) {
	fmt.Print("before mount func thing\n")
	mnt, err := fstestutil.MountedFuncT(t, mount, nil)
	if err != nil {
		t.Error(err)
	}
	defer func() {
		mnt.Close()
	}()
	if mnt == nil {
		t.Error("nil mnt")
	}

	fmt.Print("before root check\n")
	root := mnt.Dir
	fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":  neverErr,
		"dir two":  neverErr,
		"file one": neverErr,
	})

	fstestutil.CheckDir(path.Join(root, "dir one"), map[string]fstestutil.FileInfoCheck{})

	fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	})

	verifyFileContents(t, path.Join(root, "file one"), []byte("content for file one"))
	verifyFileContents(t, path.Join(root, "dir two", "file two"), []byte("content for file two"))
}

func verifyFileContents(t *testing.T, path string, expected []byte) {
	found, err := ioutil.ReadFile(path)
	if err != nil {
		t.Errorf("Error reading %q: %v", path, err)
	}
	if !bytes.Equal(found, expected) {
		t.Errorf("file %q contained %q when we expected %q", path, found, expected)
	}
}
