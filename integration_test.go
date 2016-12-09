package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
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
	ok(t, err)
	assert(t, mnt != nil, "nil mnt")

	return mnt, sys
}

func TestCreateAndClose(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()
	root := mnt.Dir

	ok(t, fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	}))

	fp := path.Join(root, "dir two", "amanda.txt")
	file, err := os.Create(fp)
	defer close(file)
	ok(t, err)
	file.Close()

	ok(t, fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two":   neverErr,
		"amanda.txt": neverErr,
	}))
}

func TestCreateWriteAndClose(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()
	root := mnt.Dir

	ok(t, fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	}))

	var err error
	fp := path.Join(root, "dir two", "amanda.txt")
	var file *os.File
	file, err = os.Create(fp)
	defer close(file)
	ok(t, err)

	fi, err := file.Stat()
	ok(t, err)
	equals(t, int64(0), fi.Size())

	_, err = file.WriteString("written for amanda")
	ok(t, err)

	fi, err = file.Stat()
	ok(t, err)
	equals(t, int64(len([]byte("written for amanda"))), fi.Size())

	ok(t, file.Close())

	ok(t, fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two":   neverErr,
		"amanda.txt": neverErr,
	}))
	verifyFileContents(t, path.Join(root, "dir two", "amanda.txt"), "written for amanda")
}

func TestRename(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()

	root := mnt.Dir
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":  neverErr,
		"dir two":  neverErr,
		"file one": neverErr,
	}))

	// test moving withing the same directory
	ok(t, os.Rename(path.Join(root, "file one"), path.Join(root, "file one.one")))
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":      neverErr,
		"dir two":      neverErr,
		"file one.one": neverErr,
	}))

	// test moving to a new directory
	ok(t, os.Rename(path.Join(root, "file one.one"), path.Join(root, "dir one", "file one.one")))
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one": neverErr,
		"dir two": neverErr,
	}))
	ok(t, fstestutil.CheckDir(path.Join(root, "dir one"), map[string]fstestutil.FileInfoCheck{
		"file one.one": neverErr,
	}))
}

func TestRemove(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()

	root := mnt.Dir
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":  neverErr,
		"dir two":  neverErr,
		"file one": neverErr,
	}))

	// test moving withing the same directory
	ok(t, os.Remove(path.Join(root, "file one")))
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one": neverErr,
		"dir two": neverErr,
	}))

	// test moving withing the same directory
	ok(t, os.Remove(path.Join(root, "dir one")))
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir two": neverErr,
	}))
}

func TestMkdir(t *testing.T) {
	mnt, _ := testMount(t, false)
	defer func() {
		mnt.Close()
	}()

	root := mnt.Dir
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":  neverErr,
		"dir two":  neverErr,
		"file one": neverErr,
	}))

	// test moving withing the same directory
	ok(t, os.Mkdir(path.Join(root, "dir three"), 0700))
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":   neverErr,
		"dir two":   neverErr,
		"dir three": neverErr,
		"file one":  neverErr,
	}))
}

func TestChanges(t *testing.T) {
	mnt, sys := testMount(t, true)
	defer func() {
		mnt.Close()
	}()

	fmt.Print("before root check\n")
	root := mnt.Dir
	ok(t, fstestutil.CheckDir(root, map[string]fstestutil.FileInfoCheck{
		"dir one":  neverErr,
		"dir two":  neverErr,
		"file one": neverErr,
	}))

	ok(t, fstestutil.CheckDir(path.Join(root, "dir one"), map[string]fstestutil.FileInfoCheck{}))

	ok(t, fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	}))

	verifyFileContents(t, path.Join(root, "file one"), "content for file_one_id")
	verifyFileContents(t, path.Join(root, "dir two", "file two"), "content for file_two_id")

	createFileThreeChange := gdrive.Change{
		ID:      "file_three_id",
		Removed: false,
		Node:    fakedrive.MakeTextFile("file_three_id", "file three", "dir_two_id"),
	}
	cs := gdrive.ChangeStats{}
	equals(t, gdrive.ChangeStats{}, cs)
	sys.processChange(&createFileThreeChange, &cs)
	ok(t, fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two":   neverErr,
		"file three": neverErr,
	}))
	verifyFileContents(t, path.Join(root, "dir two", "file three"), "content for file_three_id")
	equals(t, gdrive.ChangeStats{Changed: 1, Ignored: 0}, cs)

	rmFileThreeChange := gdrive.Change{
		ID:      "file_three_id",
		Removed: true,
		Node:    nil,
	}
	sys.processChange(&rmFileThreeChange, &cs)
	ok(t, fstestutil.CheckDir(path.Join(root, "dir two"), map[string]fstestutil.FileInfoCheck{
		"file two": neverErr,
	}))
	equals(t, gdrive.ChangeStats{Changed: 2, Ignored: 0}, cs)
}

func verifyFileContents(t *testing.T, path string, expected string) {
	found, err := ioutil.ReadFile(path)
	ok(t, err)
	equals(t, []byte(expected), found)
}

// assert fails the test if the condition is false.
func assert(tb testing.TB, condition bool, msg string, v ...interface{}) {
	if !condition {
		_, file, line, _ := runtime.Caller(1)
		fmt.Printf("\033[31m%s:%d: "+msg+"\033[39m\n\n", append([]interface{}{filepath.Base(file), line}, v...)...)
		tb.FailNow()
	}
}

// ok fails the test if an err is not nil.
func ok(tb testing.TB, err error) {
	if err != nil {
		_, file, line, _ := runtime.Caller(1)
		fmt.Printf("\033[31m%s:%d: unexpected error: %s\033[39m\n\n", filepath.Base(file), line, err.Error())
		tb.FailNow()
	}
}

// equals fails the test if exp is not equal to act.
func equals(tb testing.TB, exp, act interface{}) {
	if !reflect.DeepEqual(exp, act) {
		_, file, line, _ := runtime.Caller(1)
		fmt.Printf("\033[31m%s:%d:\n\n\texp: %#v\n\n\tgot: %#v\033[39m\n\n", filepath.Base(file), line, exp, act)
		tb.FailNow()
	}
}

// close is meant to be called in defer, after opening a file.
// Necessary in the case where a test fails early with a
// FailNow.  If the test ran successfully than this will be a
// duplicate call that will return syscall.EINVAL, which we ignore
func close(f *os.File) {
	f.Close()
}
