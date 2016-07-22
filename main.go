// Hellofs implements a simple "hello world" file system.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/ginabythebay/mnt-gdrive/internal/gdrive"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	_ "bazil.org/fuse/fs/fstestutil"
	"golang.org/x/net/context"
)

const (
	// Not sure if something in the kernel or fuse might get upset by a zero value, so we
	// skip past it.
	reservedIdx = iota

	// Group of special fixed indices for 'magic' files that aren't part of gdrive and a
	// therefore outside of our normal allocation mechanism.
	dumpIdx

	// Where we start allocating indices for gdrive files
	firstDynamicIdx
)

// TODO(gina) do something realz here
const MODE_FILE = 0444
const MODE_DIR = os.ModeDir | 0555

// TODO(gina) make this configurable
const changeFetchSleep = time.Duration(5) * time.Second

// The handle that the kernel expects to use when identifying files and directories.  The
// kernel often calls this inode.  But it also uses inode but since inode is also used to
// refer to the struct that many filesystems use, that seems confusing.
type index uint64

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s MOUNTPOINT\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	mountpoint := flag.Arg(0)

	gdriveService, err := gdrive.GetService()
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

	config := fs.Config{
		Debug: func(msg interface{}) {
			log.Print(msg)
		},
		WithContext: func(ctx context.Context, req fuse.Request) context.Context {
			return ctx
		},
	}

	server := fs.New(c, &config)
	err = server.Serve(wrapper{gdriveService, server})
	if err != nil {
		log.Fatal(err)
	}

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		log.Fatal(err)
	}
}

type wrapper struct {
	gdriveService *drive.Service
	server        *fs.Server
}

func (w wrapper) Root() (fs.Node, error) {
	startPageToken, err := gdrive.GetStartPageToken(w.gdriveService)
	if err != nil {
		log.Fatalf("Unable to fetch startPageToken, %v", err)
	}

	g, err := gdrive.FetchNode(w.gdriveService, "root")
	if err != nil {
		log.Print("Error fetching root", err)
		return nil, fuse.ENODATA
	}

	s := &system{
		gdriveService: w.gdriveService,
		server:        w.server,
		changesToken:  startPageToken,
		nextInode:     firstDynamicIdx,
		serverStart:   time.Now(),
		updateTime:    time.Now(),
		idMap:         make(map[string]*node),
		inodeMap:      make(map[index]*node)}

	root := s.getOrMakeNode(g)

	go s.watchForChanges()

	return root, nil
}

// FS implements the hello world file system.
type system struct {
	gdriveService *drive.Service
	server        *fs.Server

	// there is a single goroutine that reads/updates this, so it isn't guarded
	changesToken string

	// guards all of the fields below
	mu sync.Mutex

	nextInode index

	serverStart time.Time
	updateTime  time.Time

	// maps from google drive id to node
	idMap map[string]*node
	// maps from inode number to node
	inodeMap map[index]*node

	initDumpOnce sync.Once
	dumpNode     *dumpNodeType
}

func (s *system) watchForChanges() {
	// TODO(gina) provide a way to cancel this via context?

	// TODO(gina) Better to select on a channel that we send ticks to.
	// Then when something updates the filesystem from our side, we
	// can run this right away to see the result.

	// TODO(gina) track the last time we fetched changes without an error, use that to
	// determine staleness elsewhere, to .e.g. shutdown the system if this seems borken
	for {
		time.Sleep(changeFetchSleep)

		sum, err := gdrive.ProcessChanges(s.gdriveService, &s.changesToken,
			s.processChange)
		if err != nil {
			if sum > 0 {
				log.Fatalf("Aborting due to failure to fetch changes partway through change processing.  We don't support idempotent operations so cannot continue: %v", err)
			} else {
				log.Printf("Failed to fetch changes.  Will try again later: %v", err)
			}
		}
		if sum > 0 {
			log.Printf("successfully applied %d changes", sum)
		}
	}
}

func (s *system) processChange(c *gdrive.Change) (changeCount uint32) {
	trash := c.Removed || c.Node.Trashed
	s.mu.Lock()
	defer s.mu.Unlock()

	n, nodeExists := s.idMap[c.ID]
	//
	// If I hold onto the system lock, I need rework the code below to make sure it
	// doesn't try to grab it if called from this call path.

	switch {
	case trash:
		if nodeExists {
			s.removeNode(n)
			n.server.InvalidateNodeData(n)
			log.Printf("Removed %s", c.ID)
			changeCount = 1
		}
	case nodeExists && !c.Node.IncludeNode():
		// This can happen if a file got renamed to contain a slash, or if it was owned
		// by the user but is now not
		s.removeNode(n)
		n.server.InvalidateNodeData(n)
		log.Printf("Removed %s", c.ID)
		changeCount = 1
	case nodeExists:
		// TODO(gina) this is more aggressive than needed.  If only
		// metadata changed, we don't need to invalidate the content
		// entry
		if !n.dir {
			n.server.InvalidateNodeData(n)
		}
		n.update(c.Node)
		log.Printf("Updated %d/%s", n.idx, c.ID)
		changeCount = 1
	default:
		// We want to create this new node if there is at least one of
		// our parents has children
		var haveReadyParent bool
		for _, pid := range c.Node.ParentIDs {
			if p, ok := s.idMap[pid]; ok && p.haveChildren() {
				haveReadyParent = true
				break
			}
		}
		if haveReadyParent {
			s.insertNode(c.Node)
			log.Printf("Created %s because a parent needed to know about it", c.ID)
			changeCount = 1
		} else {
			log.Printf("Ignoring unkown id %s", c.ID)
		}
	}
	return changeCount
}

// assumes we already have the system lock
func (s *system) removeNode(n *node) {
	delete(s.idMap, n.id)
	delete(s.inodeMap, n.idx)
	s.updateTime = time.Now()

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

// TODO(gina) I think it would make sense to have this instead return a tuple of
// (*node, idx) where if *node is nil, then the idx will be the value to assign to a new node.
func (s *system) getOrMakeNode(g *gdrive.Node) *node {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.idMap[g.ID]
	if !ok {
		n = s.insertNode(g)
	} else {
		n.update(g)
	}
	s.updateTime = time.Now()

	return n
}

// Assumes the system lock is already held
func (s *system) insertNode(g *gdrive.Node) *node {
	s.nextInode++
	inode := s.nextInode
	pm := map[string]*node{}
	for _, id := range g.ParentIDs {
		if p, ok := s.idMap[id]; ok {
			pm[id] = p
		}
	}
	n := newNode(s, inode, g, pm)
	for _, p := range pm {
		if p.haveChildren() {
			p.mu.Lock()
			p.children[n.id] = n
			p.mu.Unlock()
		}
	}
	s.inodeMap[inode] = n
	s.idMap[g.ID] = n
	return n
}

type node struct {
	*system
	// These are things we expect to be immutable for a node
	idx index
	id  string

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

func newNode(s *system, idx index, g *gdrive.Node, parents map[string]*node) *node {
	return &node{system: s, idx: idx, id: g.ID, name: g.Name, ctime: g.Ctime, mtime: g.Mtime, size: g.Size, version: g.Version, dir: g.Dir(), parents: parents}
}

type printableNode struct {
	name    string
	dir     bool
	idx     index
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
	p := printableNode{n.name, n.dir, n.idx, n.id, n.ctime, n.mtime, n.size, n.version}
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

func (n *node) update(g *gdrive.Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.name = g.Name
	n.ctime = g.Ctime
	n.mtime = g.Mtime
	n.size = g.Size
	n.version = g.Version
	n.dir = g.Dir()

	newParentSet := map[string]bool{}
	for _, id := range g.ParentIDs {
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
	a.Inode = uint64(n.idx)
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

	gs, err := gdrive.FetchChildren(ctx, n.gdriveService, n.id)
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

		ds = append(ds, fuse.Dirent{uint64(c.idx), dt, c.name})
	}

	log.Printf("ReadDirAll returning %d children", len(ds))
	return ds, nil
}

func (n *node) Lookup(ctx context.Context, name string) (ret fs.Node, err error) {
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

var _ fs.NodeOpener = (*node)(nil)

func (n *node) Open(ctx context.Context, req *fuse.OpenRequest, res *fuse.OpenResponse) (handle fs.Handle, err error) {
	if !req.Flags.IsReadOnly() {
		return nil, fuse.Errno(syscall.EACCES)
	}

	if n.dir {
		// send the caller to ReadDirAll
		return n, nil
	}

	res.Flags |= fuse.OpenKeepCache

	return &fileReader{n: n}, nil
}

var _ fs.HandleReader = (*fileReader)(nil)
var _ fs.HandleReleaser = (*fileReader)(nil)

type fileReader struct {
	n       *node
	init    sync.Once
	tmpFile *os.File
}

func (r *fileReader) fetch() {
	tmpFile, err := ioutil.TempFile("", r.n.name)
	if err != nil {
		log.Printf("Error creating temp file for %s: %v", r.n.name, err)
		return
	}
	defer func() {
		if r.tmpFile == nil {
			tmpFile.Close()
		}
	}()

	err = gdrive.Download(r.n.gdriveService, r.n.id, tmpFile)
	if err != nil {
		return
	}
	r.tmpFile = tmpFile
}

func (r *fileReader) Read(ctx context.Context, req *fuse.ReadRequest, res *fuse.ReadResponse) error {
	r.init.Do(r.fetch)
	if r.tmpFile == nil {
		return fuse.EIO
	}
	b := make([]byte, req.Size)
	n, err := r.tmpFile.ReadAt(b, req.Offset)
	if err != nil && err != io.EOF {
		log.Printf("Error reading from temp file: %v", err)
		return fuse.EIO
	}
	res.Data = b[:n]
	return nil
}

func (r *fileReader) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if r.tmpFile == nil {
		return nil
	}
	err := r.tmpFile.Close()
	name := r.tmpFile.Name()
	if err != nil {
		log.Printf("Error closing %s: %v", name, err)
	}
	os.Remove(name)
	return err
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
	a.Inode = dumpIdx
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
