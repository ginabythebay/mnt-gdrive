// Hellofs implements a simple "hello world" file system.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	_ "bazil.org/fuse/fs/fstestutil"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"

	"golang.org/x/oauth2"
)

// TODO(gina) things to address
// . Need to support inodes.  Looks like if I populate it properly
//   everywhere, it will disambiguate files with the same name.  my
//   google drive is full of these.  I think that doing this will mean I
//   need to maintain a map from inode to drive id.  Which means a
//   file-system-global structure we pass around and mutex-mediated
//   access to it
// . better file modes
// . consider readonly mounting mode.  would affect flags we return in attributes,
//   whether we allow opens for writes, and whether we ask google for full access.
// . caching?
// . prefetch of extra file attributes during ReadDirAll?

const pageSize = 1000

// TODO(gina) do something realz here
const MODE_FILE = 0444
const MODE_DIR = os.ModeDir | 0555

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s MOUNTPOINT\n", os.Args[0])
	flag.PrintDefaults()
}

// nameOK returns true if the name isn't likely to upset our host.
func nameOK(name string) bool {
	return !strings.Contains(name, "/")
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	mountpoint := flag.Arg(0)

	srv, err := getDriveService()
	if err != nil {
		log.Fatal(err)
	}

	c, err := fuse.Mount(
		mountpoint,
		fuse.FSName("mntgdrive"),
		fuse.Subtype("mntgrdrivefs"),
		fuse.LocalVolume(),
		fuse.VolumeName("GDrive"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	log.Print("Entering Serve")

	err = fs.Serve(c, driveWrapper{srv})
	if err != nil {
		log.Fatal(err)
	}

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		log.Fatal(err)
	}
}

type driveWrapper struct {
	srv *drive.Service
}

func (d driveWrapper) Root() (fs.Node, error) {
	g, err := fetchGnode(d.srv, "root")
	if err != nil {
		log.Print("Error fetching root", err)
		return nil, fuse.ENODATA
	}
	s := &system{srv: d.srv, idMap: make(map[string]*node),
		inodeMap: make(map[uint64]*node)}
	n := s.getOrMakeNode(g)

	return n, nil
}

// FS implements the hello world file system.
type system struct {
	srv *drive.Service

	// guards all of the fields below
	mu sync.Mutex

	nextInode uint64

	// maps from inode to node
	inodeMap map[uint64]*node
	// maps from google drive id to node
	idMap map[string]*node
}

func (s *system) getOrMakeNode(g *gnode) *node {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.idMap[g.id]
	if !ok {
		s.nextInode++
		inode := s.nextInode
		pm := map[string]*node{}
		for _, id := range g.parentIds {
			if p, ok := s.idMap[id]; ok {
				pm[id] = p
			}
		}
		n = newNode(s, inode, g, pm)
		s.inodeMap[inode] = n
		s.idMap[g.id] = n
	} else {
		n.updateMetadata(g)
	}

	return n
}

type node struct {
	*system
	// These are things we expect to be immutable for a node
	inode uint64
	id    string

	//
	// These can change while a node exists
	//

	// directly retrieved metadata

	// guards this access to this group
	mu      sync.Mutex
	name    string
	ctime   time.Time
	mtime   time.Time
	size    uint64
	version int64
	dir     bool
	parents map[string]*node

	// guards children
	cmu sync.Mutex
	// if nil, we don't yet have children information
	children map[string]*node
}

func newNode(s *system, inode uint64, g *gnode, parents map[string]*node) *node {
	return &node{system: s, inode: inode, id: g.id, name: g.name, ctime: g.ctime, mtime: g.mtime, size: g.size, version: g.version, dir: g.dir(), parents: parents}

}

func (*node) updateMetadata(g *gnode) {
	log.Fatalf("implement updateMetadata: %#v", g)
	// locking
	// update main metadata

	// parents
	// first go through existing parents and remove us as children
	// add ourselves as a child to the new set of parents
	// record our parents
}

func (n *node) Attr(ctx context.Context, a *fuse.Attr) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	a.Inode = n.inode
	a.Size = n.size
	a.Ctime = n.ctime
	a.Crtime = n.ctime
	a.Mtime = n.mtime

	if n.dir {
		a.Mode = os.ModeDir
	}
	a.Mode |= 0400

	return nil
}

func (n *node) loadChildrenIfEmpty(ctx context.Context) error {
	n.cmu.Lock()
	loaded := n.children != nil
	n.cmu.Unlock()
	if loaded {
		return nil
	}

	gs, err := fetchGnodeChildren(ctx, n.srv, n.id)
	if err != nil {
		return err
	}

	var children []*node
	for _, g := range gs {
		// Note inside this loop we are aquiring and releasing a lock over and over.  I don't know if that is bad yet.
		c := n.getOrMakeNode(g)
		c.addParent(n)
		children = append(children, c)
	}

	childMap := map[string]*node{}
	for _, c := range children {
		childMap[c.id] = c
	}

	n.cmu.Lock()
	n.children = childMap
	n.cmu.Unlock()
	return nil
}

func (n *node) addParent(p *node) {
	n.mu.Lock()
	n.parents[p.id] = p
	n.mu.Unlock()
}

func (n *node) ReadDirAll(ctx context.Context) (ds []fuse.Dirent, err error) {
	if err = n.loadChildrenIfEmpty(ctx); err != nil {
		log.Printf("Unable to retrieve children of %v due to %s", n.id, err)
		return nil, fuse.EIO
	}

	n.cmu.Lock()
	defer n.cmu.Unlock()
	for _, c := range n.children {
		var dt fuse.DirentType
		if c.dir {
			dt = fuse.DT_Dir
		} else {
			dt = fuse.DT_File
		}

		ds = append(ds, fuse.Dirent{c.inode, dt, c.name})
	}

	log.Printf("ReadDirAll returning %d children", len(ds))
	return ds, nil
}

// // Dir implements both Node and Handle for the root directory.
// type Dir struct {
// 	*node
// }

// func (Dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
// 	if name == "hello" {
// 		return File{}, nil
// 	}
// 	return nil, fuse.ENOENT
// }

// func (d Dir) NewDirEnt(id string, name string, t fuse.DirentType) fuse.Dirent {
// 	return fuse.Dirent{Inode: d.getInode(id), Name: name, Type: t}
// }

// func (d Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
// 	result := make([]fuse.Dirent, 0)

// 	handler := func(r *drive.FileList) error {
// 		for _, f := range r.Files {
// 			if !nameOK(f.Name) {
// 				continue
// 			}

// 			var fsType fuse.DirentType
// 			if isDir(f) {
// 				fsType = fuse.DT_Dir
// 			} else {
// 				fsType = fuse.DT_File
// 			}

// 			result = append(result, d.NewDirEnt(f.Id, f.Name, fsType))
// 		}
// 		return nil
// 	}

// 	err := d.srv.Files.List().
// 		PageSize(pageSize).
// 		Fields("nextPageToken, files(id, name, fileExtension, mimeType)").
// 		Q(fmt.Sprintf("'%s' in parents and trashed = false", d.id)).
// 		Pages(ctx, handler)
// 	if err != nil {
// 		log.Print("Unable to retrieve files.", err)
// 		return nil, fuse.ENODATA
// 	}

// 	return result, nil
// }

// File implements both Node and Handle for the hello file.
type File struct{}

const greeting = "hello, world\n"

func (File) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = MODE_FILE
	a.Size = uint64(len(greeting))
	return nil
}

func (File) ReadAll(ctx context.Context) ([]byte, error) {
	return []byte(greeting), nil
}

////////////////////////////////////////////////////////////////////////
// GDRIVE SUPPORT BELOW
////////////////////////////////////////////////////////////////////////

// Returns the drive service
func getDriveService() (*drive.Service, error) {
	ctx := context.Background()

	b, err := ioutil.ReadFile("client_secret.json")
	if err != nil {
		return nil, fmt.Errorf("Unable to read client secret file: %v", err)
	}

	// If modifying these scopes, delete your previously saved credentials
	// at ~/.credentials/drive-go-quickstart.json
	config, err := google.ConfigFromJSON(b, drive.DriveReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse client secret file to config: %v", err)
	}
	client := getClient(ctx, config)

	return drive.New(client)
}

// getClient uses a Context and Config to retrieve a Token
// then generate a Client. It returns the generated Client.
func getClient(ctx context.Context, config *oauth2.Config) *http.Client {
	cacheFile, err := tokenCacheFile()
	if err != nil {
		log.Fatalf("Unable to get path to cached credential file. %v", err)
	}
	tok, err := tokenFromFile(cacheFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(cacheFile, tok)
	}
	return config.Client(ctx, tok)
}

// getTokenFromWeb uses Config to request a Token.
// It returns the retrieved Token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		log.Fatalf("Unable to read authorization code %v", err)
	}

	tok, err := config.Exchange(oauth2.NoContext, code)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web %v", err)
	}
	return tok
}

// tokenCacheFile generates credential file path/filename.
// It returns the generated credential path/filename.
func tokenCacheFile() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	tokenCacheDir := filepath.Join(usr.HomeDir, ".credentials")
	os.MkdirAll(tokenCacheDir, 0700)
	return filepath.Join(tokenCacheDir,
		url.QueryEscape("mnt-gdrive.json")), err
}

// tokenFromFile retrieves a Token from a given file path.
// It returns the retrieved Token and any read error encountered.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	t := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(t)
	defer f.Close()
	return t, err
}

// saveToken uses a file path to create a file and store the
// token in it.
func saveToken(file string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", file)
	f, err := os.Create(file)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

// gnode represents raw metadata about a file or directory that came from google drive.
// Mostly a simple data-holder
type gnode struct {
	// should never change
	id string

	name      string
	ctime     time.Time
	mtime     time.Time
	size      uint64
	version   int64
	parentIds []string
	trashed   bool

	// We use these to determine if it is a folder
	fileExtension string
	mimeType      string
}

func makeGnode(id string, f *drive.File) (*gnode, error) {
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

	return &gnode{id,
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

var fileFields googleapi.Field = "id, name, createdTime, modifiedTime, size, version, parents, fileExtension, mimeType, trashed"
var childFields = "nextPageToken, files(" + fileFields + ")"

func fetchGnode(srv *drive.Service, id string) (g *gnode, err error) {
	f, err := srv.Files.Get(id).
		Fields(fileFields).
		Do()
	if err != nil {
		log.Print("Unable to fetch dir info.", err)
		return nil, fuse.ENODATA
	}
	if !nameOK(f.Name) || f.Trashed {
		return nil, fuse.ENODATA
	}

	return makeGnode(id, f)
}

func fetchGnodeChildren(ctx context.Context, srv *drive.Service, id string) (gs []*gnode, err error) {
	handler := func(r *drive.FileList) error {
		for _, f := range r.Files {
			if !nameOK(f.Name) {
				continue
			}
			// if there was an error in makeGnode, we logged it and we will just skip it here
			if g, err := makeGnode(f.Id, f); err == nil {
				gs = append(gs, g)
			}
		}
		return nil
	}

	err = srv.Files.List().
		PageSize(pageSize).
		Fields(childFields).
		Q(fmt.Sprintf("'%s' in parents and trashed = false", id)).
		Pages(ctx, handler)
	if err != nil {
		log.Print("Unable to retrieve files.", err)
		return nil, fuse.ENODATA
	}
	return gs, nil
}

func (g *gnode) dir() bool {
	// see https://developers.google.com/drive/v3/web/folder
	if g.mimeType == "application/vnd.google-apps.folder" && g.fileExtension == "" {
		return true
	}
	return false
}
