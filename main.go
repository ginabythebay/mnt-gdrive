// Hellofs implements a simple "hello world" file system.
package main

import (
	"bytes"
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

const (
	reservedInode = iota
	dumpInode
	firstDynamicInode
)

// TODO(gina) do something realz here
const MODE_FILE = 0444
const MODE_DIR = os.ModeDir | 0555

const fileFields = "id, name, createdTime, modifiedTime, size, version, parents, fileExtension, mimeType, trashed"
const fileGroupFields = "nextPageToken, files(" + fileFields + ")"

// TODO(gina) make this configurable
const changeFetchSleep = time.Duration(5) * time.Second

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
	token, err := d.srv.Changes.GetStartPageToken().Do()
	if err != nil {
		log.Fatalf("Unable to fetch startPageToken, %q", err)
	}

	g, err := fetchGnode(d.srv, "root")
	if err != nil {
		log.Print("Error fetching root", err)
		return nil, fuse.ENODATA
	}

	s := &system{
		srv:          d.srv,
		changesToken: token.StartPageToken,
		nextInode:    firstDynamicInode,
		serverStart:  time.Now(),
		updateTime:   time.Now(),
		idMap:        make(map[string]*node),
		inodeMap:     make(map[uint64]*node)}

	root := s.getOrMakeNode(g)

	go s.watchForChanges()

	return root, nil
}

// FS implements the hello world file system.
type system struct {
	srv *drive.Service

	// there is a single goroutine that reads/updates this, so it isn't guarded
	changesToken string

	// guards all of the fields below
	mu sync.Mutex

	nextInode uint64

	serverStart time.Time
	updateTime  time.Time

	// maps from inode to node
	inodeMap map[uint64]*node
	// maps from google drive id to node
	idMap map[string]*node

	initDumpOnce sync.Once
	dumpNode     *dumpNodeType
}

func (s *system) watchForChanges() {
	// TODO(gina) provide a way to cancel this via context?

	// TODO(gina) track the last time we fetched changes without an error, use that to
	// determine staleness elsewhere, to .e.g. shutdown the system if this seems borken
	for {
		time.Sleep(changeFetchSleep)
		pageToken := s.changesToken
		count := 0
		for pageToken != "" {
			// TODO(gina) see if we can reduce notification spam.  Right now I believe
			// we are getting notified every time the view time for something gets updated
			// and that isn't useful.  Maybe we can exclude that field and get fewer
			// notifications.
			request := s.srv.Changes.List(pageToken).
				IncludeRemoved(true).
				RestrictToMyDrive(true).
				Fields("changes/*,kind,newStartPageToken,nextPageToken")
			cl, err := request.
				Do()
			if err != nil {
				log.Printf("Error fetching changes, will continue trying: %v", err)
				break
			}
			for _, ch := range cl.Changes {
				s.processChange(ch)
				count++
			}
			if cl.NewStartPageToken != "" {
				s.changesToken = cl.NewStartPageToken
			}
			pageToken = cl.NextPageToken
		}
		log.Printf("Successfully applied %d changes", count)
	}
}

func (s *system) processChange(c *drive.Change) {
	trash := c.Removed || c.File.Trashed
	var parents []*node
	s.mu.Lock()
	n, nodeExists := s.idMap[c.FileId]
	if !trash && !nodeExists {
		// do this now while holding the system lock so we have it below when deciding
		// whether it makes sense to create a node for this change
		for _, pid := range c.File.Parents {
			if p, ok := s.idMap[pid]; ok {
				parents = append(parents, p)
			}
		}
	}
	s.mu.Unlock()

	// TODO(gina) there is an evil race condition lurking here.  We make decisions up
	// above while holding the system lock that might be untrue by the time we execute
	// things below.
	//
	// If I hold onto the system lock, I need rework the code below to make sure it doesn't try to grab it if called from this call path.

	switch {
	case trash:
		s.removeNode(n)
		log.Printf("Removed %s", c.FileId)
	case nodeExists:
		g, err := makeGnode(c.FileId, c.File)
		if err != nil {
			log.Fatalf("Aborting due to change we cannot handle %+v due to %v", c, err)
		}
		n.update(g)
		log.Printf("Updated %s", c.FileId)
	default:
		// We want to create this new node if there is at least one parent node in our
		// tree that has children
		var haveReadyParent bool
		for _, p := range parents {
			if p.haveChildren() {
				haveReadyParent = true
				break
			}
		}
		if haveReadyParent {
			g, err := makeGnode(c.FileId, c.File)
			if err != nil {
				log.Fatalf("Aborting due to change we cannot handle %+v due to %v", c, err)
			}
			s.getOrMakeNode(g) // creates in this case
			log.Printf("Created %s because a parent needed to know about it", c.FileId)
		} else {
			log.Printf("Ignoring unkown id %s", c.FileId)
		}
	}
}

func (s *system) removeNode(n *node) {
	s.mu.Lock()
	delete(s.idMap, n.id)
	delete(s.inodeMap, n.inode)
	s.updateTime = time.Now()
	s.mu.Unlock()

	// TODO(gina) figure out how to tell the kernel to invalidate the entry

	for _, p := range n.parents {
		// TODO(gina) figure out how to tell the kernel to invalidate the directory (parent) containing our node (the kernel cache of it)
		p.cmu.Lock()
		if _, ok := p.children[n.id]; ok {
			delete(p.children, n.id)
		} else {
			log.Fatalf("Inconsistent data: node %+v listed parent %+v, but that parent does not know about the node", n, p)
		}
		p.cmu.Unlock()
	}
}

func (s *system) getNodeIfExists(id string) *node {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _ := s.idMap[id]
	return n
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
		for _, p := range pm {
			if p.haveChildren() {
				p.mu.Lock()
				p.children[n.id] = n
				p.mu.Unlock()
			}
		}
		s.inodeMap[inode] = n
		s.idMap[g.id] = n
	} else {
		n.update(g)
	}
	s.updateTime = time.Now()

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

type printableNode struct {
	name    string
	dir     bool
	inode   uint64
	id      string
	ctime   time.Time
	mtime   time.Time
	size    uint64
	version int64
}

const indent = 2

func (n *node) dump(b *bytes.Buffer, level int) {
	margin := strings.Repeat(" ", level*indent)
	b.WriteString(margin)

	n.mu.Lock()
	p := printableNode{n.name, n.dir, n.inode, n.id, n.ctime, n.mtime, n.size, n.version}
	n.mu.Unlock()
	b.WriteString(fmt.Sprintf("%#v\n", p))
	if p.dir {
		if n.haveChildren() {
			// we build a separate list of children so we don't have to acquire locks all
			// the way down, which could lead to deadlock if an update comes in that
			// expects locking upward.
			//
			// TODO(gina) rework locking order to go downward(?)
			var children []*node
			n.cmu.Lock()
			for _, c := range n.children {
				children = append(children, c)
			}
			n.cmu.Unlock()
			for _, c := range children {
				c.dump(b, level+1)
			}
		} else {
			margin = strings.Repeat(" ", (level+1)*indent)
			b.WriteString(fmt.Sprintf("%s<unknown children>\n", margin))
		}
	}
}

func (n *node) update(g *gnode) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.name = g.name
	n.ctime = g.ctime
	n.mtime = g.mtime
	n.size = g.size
	n.version = g.version
	n.dir = g.dir()

	newParentSet := map[string]bool{}
	for _, id := range g.parentIds {
		newParentSet[id] = true
	}

	// loop through existing parents looking for ones no longer present and tell them to
	// remove us
	for _, ep := range n.parents {
		if _, ok := newParentSet[ep.id]; !ok {
			ep.removeChild(n.id)
			delete(n.parents, ep.id)
		}
	}

	// loop through new parents, looking for ones that aren't yet present and tell them
	// to add us
	for np := range newParentSet {
		if _, ok := n.parents[np]; !ok {
			if p := n.getNodeIfExists(np); p != nil {
				p.addChild(n)
				n.parents[np] = p
			}
		}
	}
	n.updateTime = time.Now()
}

func (n *node) addChild(c *node) {
	n.cmu.Lock()
	defer n.cmu.Unlock()
	n.children[c.id] = c
	n.updateTime = time.Now()
}

func (n *node) removeChild(id string) {
	n.cmu.Lock()
	defer n.cmu.Unlock()
	delete(n.children, id)
	n.updateTime = time.Now()
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
		a.Mode = MODE_DIR
	} else {
		a.Mode = MODE_FILE
	}

	return nil
}

func (n *node) haveChildren() bool {
	n.cmu.Lock()
	loaded := n.children != nil
	n.cmu.Unlock()
	return loaded
}

func (n *node) loadChildrenIfEmpty(ctx context.Context) error {
	if n.haveChildren() {
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

	n.mu.Lock()
	n.updateTime = time.Now()
	n.mu.Unlock()

	return nil
}

func (n *node) addParent(p *node) {
	n.mu.Lock()
	n.parents[p.id] = p
	n.updateTime = time.Now()
	n.mu.Unlock()
}

func (n *node) ReadDirAll(ctx context.Context) (ds []fuse.Dirent, err error) {
	if err = n.loadChildrenIfEmpty(ctx); err != nil {
		return nil, err
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

func (n *node) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if err := n.loadChildrenIfEmpty(ctx); err != nil {
		return nil, err
	}

	if n.id == "root" && name == ".dump" {
		n.initDumpOnce.Do(func() {
			n.dumpNode = &dumpNodeType{n}
		})
		return n.dumpNode, nil
	}

	n.cmu.Lock()
	defer n.cmu.Unlock()
	for _, c := range n.children {
		if c.name == name {
			return c, nil
		}
	}
	return nil, fuse.ENOENT
}

func (n *node) ReadAll(ctx context.Context) (result []byte, err error) {
	start := time.Now()
	n.mu.Lock()
	dir := n.dir
	size := n.size
	id := n.id
	n.mu.Unlock()

	if dir {
		return nil, fuse.ENOTSUP
	}
	if size == 0 {
		return []byte{}, nil
	}

	resp, err := n.srv.Files.Get(id).Download()
	defer resp.Body.Close()
	defer func() {
		log.Printf("reading %d bytes took %s", len(result), time.Since(start))
	}()
	if err != nil {
		log.Print("Unable to download.", err)
		return nil, fuse.ENODATA
	}

	// TODO(gina) better to process this in chunks, paying attention to whether ctx is canceled.
	// even better to instead support
	return ioutil.ReadAll(resp.Body)
}

type dumpNodeType struct {
	root *node
}

func (d *dumpNodeType) text() string {
	var b bytes.Buffer
	d.root.dump(&b, 0)
	return b.String()
}

func (d *dumpNodeType) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = dumpInode
	a.Size = uint64(len(d.text()))
	a.Mode = MODE_FILE

	d.root.mu.Lock()
	a.Ctime = d.root.serverStart
	a.Crtime = d.root.serverStart
	a.Mtime = d.root.updateTime
	d.root.mu.Unlock()

	return nil
}

func (d *dumpNodeType) ReadAll(ctx context.Context) (result []byte, err error) {
	return []byte(d.text()), nil
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

func fetchGnode(srv *drive.Service, id string) (g *gnode, err error) {
	f, err := srv.Files.Get(id).
		Fields(fileFields).
		Do()
	if err != nil {
		log.Print("Unable to fetch node info.", err)
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

	// TODO(gina) we need to exclude items that are not in 'my drive', to match what
	// we are doing in changes.  we could do it in the query below maybe, or filter it in
	// the handler above, where we filter on name

	err = srv.Files.List().
		PageSize(pageSize).
		Fields(fileGroupFields).
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
