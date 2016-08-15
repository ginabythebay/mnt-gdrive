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

func testMount(t *testing.T, readonly bool) (*fstestutil.Mount, *system) {
	var sys *system
	mntFunc := func(mnt *fstestutil.Mount) fs.FS {
		sys = newSystem(fakedrive.NewDrive(allNodes()), mnt.Server, readonly)
		return sys
	}
	mnt, err := fstestutil.MountedFuncT(t, mntFunc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mnt == nil {
		t.Fatal("nil mnt")
	}

	return mnt, sys
}

func TestCreateAndClose(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()
	root := mnt.Dir

	err := fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	})
	if err != nil {
		t.Fatal(err)
	}

	fp := path.Join(root, "dir two", "amanda.txt")
	file, err := os.Create(fp)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()

	err = fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two":   neverErr,
		"amanda.txt": neverErr,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateWriteAndClose(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()
	root := mnt.Dir

	var err error
	if err = fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	}); err != nil {
		t.Error(err)
		return
	}

	fp := path.Join(root, "dir two", "amanda.txt")
	var file *os.File
	if file, err = os.Create(fp); err != nil {
		t.Error(err)
		return
	}
	var n int
	if n, err = file.WriteString("written for amanda"); err != nil {
		t.Errorf("Unexpected response from WriteString.  n=%d, err=%v", n, err)
		file.Close()
		return
	}
	if err = file.Close(); err != nil {
		t.Error(err)
		return
	}

	err = fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two":   neverErr,
		"amanda.txt": neverErr,
	})
	if err != nil {
		t.Error(err)
		return
	}
	verifyFileContents(t, path.Join(root, "dir two", "amanda.txt"), []byte("written for amanda"))
}

func TestRename(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()

	root := mnt.Dir
	err := fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":  neverErr,
		"dir two":  neverErr,
		"file one": neverErr,
	})
	if err != nil {
		t.Error(err)
		return
	}

	// test moving withing the same directory
	err = os.Rename(path.Join(root, "file one"), path.Join(root, "file one.one"))
	if err != nil {
		t.Error(err)
		return
	}
	err = fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":      neverErr,
		"dir two":      neverErr,
		"file one.one": neverErr,
	})
	if err != nil {
		t.Error(err)
		return
	}

	// test moving to a new directory
	err = os.Rename(path.Join(root, "file one.one"), path.Join(root, "dir one", "file one.one"))
	if err != nil {
		t.Error(err)
		return
	}
	err = fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one": neverErr,
		"dir two": neverErr,
	})
	if err != nil {
		t.Error(err)
		return
	}
	err = fstestutil.CheckDir(path.Join(root, "dir one"), map[string]fstestutil.FileInfoCheck{
		"file one.one": neverErr,
	})
	if err != nil {
		t.Error(err)
		return
	}
}

func TestRemove(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()

	root := mnt.Dir
	err := fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":  neverErr,
		"dir two":  neverErr,
		"file one": neverErr,
	})
	if err != nil {
		t.Error(err)
		return
	}

	// test moving withing the same directory
	err = os.Remove(path.Join(root, "file one"))
	if err != nil {
		t.Error(err)
		return
	}
	err = fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one": neverErr,
		"dir two": neverErr,
	})
	if err != nil {
		t.Error(err)
		return
	}

	// test moving withing the same directory
	err = os.Remove(path.Join(root, "dir one"))
	if err != nil {
		t.Error(err)
		return
	}
	err = fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir two": neverErr,
	})
	if err != nil {
		t.Error(err)
		return
	}
}

func TestChanges(t *testing.T) {
	mnt, sys := testMount(t, true)
	defer func() {
		mnt.Close()
	}()

	fmt.Print("before root check\n")
	root := mnt.Dir
	err := fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":  neverErr,
		"dir two":  neverErr,
		"file one": neverErr,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = fstestutil.CheckDir(path.Join(root, "dir one"), map[string]fstestutil.FileInfoCheck{})
	if err != nil {
		t.Fatal(err)
	}

	err = fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	})
	if err != nil {
		t.Fatal(err)
	}

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
	err = fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two":   neverErr,
		"file three": neverErr,
	})
	if err != nil {
		t.Fatal(err)
	}
	verifyFileContents(t, path.Join(root, "dir two", "file three"), []byte("content for file_three_id"))
	verifyChangeStats(t, "create", gdrive.ChangeStats{Changed: 1, Ignored: 0}, cs)

	rmFileThreeChange := gdrive.Change{
		ID:      "file_three_id",
		Removed: true,
		Node:    nil,
	}
	sys.processChange(&rmFileThreeChange, &cs)
	err = fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	})
	if err != nil {
		t.Fatal(err)
	}
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
