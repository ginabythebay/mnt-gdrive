package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
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

func pseudo_uuid() (uuid string) {

	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		fmt.Println("Error: ", err)
		return
	}

	uuid = fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])

	return
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

func contentForTextFile(id string) []byte {
	return []byte(fmt.Sprintf("content for %s", id))
}

func makeTextFile(id string, name string, parentId string) *gdrive.Node {
	parents := []string{parentId}
	n := &gdrive.Node{
		ID:            id,
		Name:          name,
		ParentIDs:     parents,
		MimeType:      "text/plain",
		FileExtension: ".txt"}
	n.Size = uint64(len(contentForTextFile(id)))
	return n
}

type fakeDrive struct {
	allNodes []*gdrive.Node
}

func (fake *fakeDrive) newId() (id string) {
	idSet := map[string]bool{}
	for _, n := range fake.allNodes {
		idSet[n.ID] = true
	}
	for {
		candidate := pseudo_uuid()
		if _, found := idSet[candidate]; !found {
			return candidate
		}
	}
}

func (fake *fakeDrive) FetchNode(id string) (n *gdrive.Node, err error) {
	for _, n := range fake.allNodes {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, fuse.ENOENT
}

func (fake *fakeDrive) CreateNode(parentID string, name string, dir bool) (n *gdrive.Node, err error) {
	id := fake.newId()
	if dir {
		n = makeDir(id, name, parentID)
	} else {
		n = makeTextFile(id, name, parentID)
	}
	return n, nil
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
	f.Write(contentForTextFile(id))
	return nil
}

func (fake *fakeDrive) ProcessChanges(changeHandler func(*gdrive.Change, *gdrive.ChangeStats)) (gdrive.ChangeStats, error) {
	log.Fatal("implement me")
	return gdrive.ChangeStats{}, fuse.EIO
}

func neverErr(fi os.FileInfo) error {
	return nil
}

func TestScenario(t *testing.T) {
	fmt.Print("before mount func thing\n")
	var sys *system
	mntFunc := func(mnt *fstestutil.Mount) fs.FS {
		sys = newSystem(&fakeDrive{allNodes()}, mnt.Server, true)
		return sys
	}
	mnt, err := fstestutil.MountedFuncT(t, mntFunc, nil)
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

	verifyFileContents(t, path.Join(root, "file one"), []byte("content for file_one_id"))
	verifyFileContents(t, path.Join(root, "dir two", "file two"), []byte("content for file_two_id"))

	createFileThreeChange := gdrive.Change{
		ID:      "file_three_id",
		Removed: false,
		Node:    makeTextFile("file_three_id", "file three", "dir_two_id"),
	}
	cs := gdrive.ChangeStats{}
	verifyChangeStats(t, "init", gdrive.ChangeStats{}, cs)
	sys.processChange(&createFileThreeChange, &cs)
	fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two":   neverErr,
		"file three": neverErr,
	})
	verifyFileContents(t, path.Join(root, "dir two", "file three"), []byte("content for file_three_id"))
	verifyChangeStats(t, "create", gdrive.ChangeStats{Changed: 1, Ignored: 0}, cs)

	rmFileThreeChange := gdrive.Change{
		ID:      "file_three_id",
		Removed: true,
		Node:    nil,
	}
	sys.processChange(&rmFileThreeChange, &cs)
	fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two":   neverErr,
		"file three": neverErr,
	})
	verifyChangeStats(t, "create", gdrive.ChangeStats{Changed: 2, Ignored: 0}, cs)
}

func verifyFileContents(t *testing.T, path string, expected []byte) {
	found, err := ioutil.ReadFile(path)
	if err != nil {
		t.Errorf("Error reading %q: %v", path, err)
		return
	}
	if !bytes.Equal(found, expected) {
		t.Errorf("file %q contained %q when we expected %q", path, found, expected)
	}
}

func verifyChangeStats(t *testing.T, name string, expected gdrive.ChangeStats, found gdrive.ChangeStats) {
	if expected != found {
		t.Errorf("Failed %q.  Expected %#v but found %#v", name, expected, found)
	}
}
