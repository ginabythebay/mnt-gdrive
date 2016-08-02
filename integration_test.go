package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/ginabythebay/mnt-gdrive/internal/fakedrive"
	"github.com/ginabythebay/mnt-gdrive/internal/gdrive"

	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
)

func init() {
	fstestutil.DebugByDefault()
}

func allNodes() []*gdrive.Node {
	return []*gdrive.Node{
		fakedrive.MakeDir("root", "", ""),
		fakedrive.MakeDir("dir_one_id", "dir one", "root"),
		fakedrive.MakeDir("dir_two_id", "dir two", "root"),
		fakedrive.MakeTextFile("file_one_id", "file one", "root"),
		fakedrive.MakeTextFile("file_two_id", "file two", "dir_two_id"),
	}
}

func neverErr(fi os.FileInfo) error {
	return nil
}

func TestScenario(t *testing.T) {
	fmt.Print("before mount func thing\n")
	var sys *system
	mntFunc := func(mnt *fstestutil.Mount) fs.FS {
		sys = newSystem(fakedrive.NewDrive(allNodes()), mnt.Server, true)
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
		Node:    fakedrive.MakeTextFile("file_three_id", "file three", "dir_two_id"),
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
