// Hellofs implements a simple "hello world" file system.
package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codegangsta/cli"
	"github.com/ginabythebay/mnt-gdrive/internal/gdrive"
	"github.com/ginabythebay/mnt-gdrive/internal/phantomfile"

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

const (
	modeReadOnly  os.FileMode = 0555
	modeReadWrite os.FileMode = 0777
)

// TODO(gina) make this configurable
const changeFetchSleep = time.Duration(5) * time.Second

// The handle that the kernel expects to use when identifying files and directories.  The
// kernel often calls this inode.  But it also uses inode but since inode is also used to
// refer to the struct that many filesystems use, that seems confusing.
type index uint64

func main() {
	sigChan := make(chan os.Signal)
	go func() {
		stacktrace := make([]byte, 8192)
		for _ = range sigChan {
			length := runtime.Stack(stacktrace, true)
			fmt.Println(string(stacktrace[:length]))
		}
	}()
	signal.Notify(sigChan, syscall.SIGQUIT)

	app := cli.NewApp()
	app.Name = "mnt-gdrive"
	app.Usage = "mount a google drive as a fuse filesystem"
	app.Action = mount
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "w, writeable",
			Usage: "Mounts drive using writeable mode"},
	}
	app.Run(os.Args)
}

func mount(ctx *cli.Context) {
	args := ctx.Args()
	switch {
	case len(args) == 0:
		log.Fatal("You must specify a single argument which is path to the directory to use as a mount point.")
	case len(args) > 1:
		log.Fatal("Too many arguments specified. You must specify a single argument which is path to the directory to use as a mount point.")
	}

	mountpoint := args.First()
	readonly := !ctx.Bool("writeable")

	gd, err := gdrive.GetService(readonly)
	if err != nil {
		log.Fatal(err)
	}

	mountOptions := []fuse.MountOption{
		fuse.FSName("mntgdrive"),
		fuse.Subtype("mntgrdrivefs"),
		fuse.LocalVolume(),
		fuse.VolumeName("GDrive"),
	}
	if readonly {
		mountOptions = append(mountOptions, fuse.ReadOnly())
	}
	c, err := fuse.Mount(mountpoint, mountOptions...)
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
	system := newSystem(gd, server, readonly)

	go system.watchForChanges()
	err = server.Serve(system)
	if err != nil {
		log.Fatal(err)
	}

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		log.Fatal(err)
	}
}

var _ fs.FS = &system{}

// FS implements the hello world file system.
type system struct {
	gd     gdrive.DriveLike
	server *fs.Server

	readonly bool

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

func newSystem(gd gdrive.DriveLike, server *fs.Server, readonly bool) *system {
	return &system{
		gd:          gd,
		server:      server,
		readonly:    readonly,
		nextInode:   firstDynamicIdx,
		serverStart: time.Now(),
		updateTime:  time.Now(),
		idMap:       make(map[string]*node),
		inodeMap:    make(map[index]*node)}

}

func (s *system) Root() (fs.Node, error) {
	g, err := s.gd.FetchNode("root")
	if err != nil {
		log.Print("Error fetching root: ", err)
		return nil, fuse.ENODATA
	}

	root := s.getOrMakeNode(g)

	return root, nil
}

func (s *system) watchForChanges() {
	// TODO(gina) provide a way to cancel this via context?

	// TODO(gina) Better to select on a channel that we send ticks to.
	// Then when something updates the filesystem from our side, we
	// can run this right away to see the result.

	// TODO(gina) track the last time we fetched changes without an error, use that to
	// determine staleness elsewhere, to .e.g. shutdown the system if this seems borken
	log.Println("entering watchForChanges")
	defer log.Println("exiting watchForChanges")
	for {
		time.Sleep(changeFetchSleep)

		cs, err := s.gd.ProcessChanges(s.processChange)
		if err != nil {
			if cs.FetchedChanges() {
				log.Fatalf("Aborting due to failure to fetch changes partway through change processing.  We don't support idempotent operations so cannot continue: %v", err)
			} else {
				log.Printf("Failed to fetch changes.  Will try again later: %v", err)
			}
		} else {
			if cs.FetchedChanges() {
				log.Print(cs.String())
			}
		}
	}
}

func (s *system) processChange(c *gdrive.Change, cs *gdrive.ChangeStats) {
	trash := c.Removed || c.Node.Trashed
	s.mu.Lock()
	defer s.mu.Unlock()

	n, nodeExists := s.idMap[c.ID]

	switch {
	case trash:
		if nodeExists {
			s.removeNode(n)
			n.server.InvalidateNodeData(n)
			log.Printf("Removed %s", c.ID)
			cs.Changed++
		}
	case nodeExists && !c.Node.IncludeNode():
		// This can happen if a file got renamed to contain a slash, or if it was owned
		// by the user but is now not
		s.removeNode(n)
		n.server.InvalidateNodeData(n)
		log.Printf("Removed %s", c.ID)
		cs.Changed++
	case nodeExists:
		// TODO(gina) this is more aggressive than needed.  If only
		// metadata changed, we don't need to invalidate the content
		// entry
		if !n.dir {
			n.server.InvalidateNodeData(n)
		}
		n.update(c.Node)
		cs.Changed++
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
			cs.Changed++
		} else {
			cs.Ignored++
			log.Printf("Ignoring unkown id %s", c.ID)
		}
	}
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

// Assumes we have the system lock already
func (s *system) getNodeIfExists(id string) *node {
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
	pf  *phantomfile.PhantomFile

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
	n := &node{
		system:  s,
		idx:     idx,
		id:      g.ID,
		name:    g.Name,
		ctime:   g.Ctime,
		mtime:   g.Mtime,
		size:    g.Size,
		version: g.Version,
		dir:     g.Dir(),
		parents: parents}
	n.pf = phantomfile.NewPhantomFile(n)
	return n
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

// Assumes we already have the system lock
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
			log.Printf("Update %q, removing %q as a parent", n.id, ep.id)
			ep.removeChild(n.id)
			delete(n.parents, ep.id)
		}
	}

	// loop through new parents, looking for ones that aren't yet present and tell them
	// to add us
	for np := range newParentSet {
		if _, ok := n.parents[np]; !ok {
			if p := n.getNodeIfExists(np); p != nil {
				log.Printf("Update %q, adding %q as a parent", n.id, np)
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

var _ fs.NodeGetattrer = (*node)(nil)

func (n *node) Getattr(ctx context.Context, eq *fuse.GetattrRequest, resp *fuse.GetattrResponse) error {
	err := n.Attr(ctx, &resp.Attr)
	log.Printf("in my Getattr, n=%s, size=%d", n, resp.Attr.Size)
	return err
}

func (n *node) Attr(ctx context.Context, a *fuse.Attr) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	a.Inode = uint64(n.idx)
	a.Size = n.size
	a.Ctime = n.ctime
	a.Crtime = n.ctime
	a.Mtime = n.mtime

	size, modTime, ok := n.pf.StatIfLocal()
	if ok {
		a.Size = uint64(size)
		a.Mtime = modTime
	}

	mode := modeReadWrite
	if n.readonly {
		mode = modeReadOnly
	}

	if n.dir {
		a.Mode = os.ModeDir | mode
	} else {
		a.Mode = mode
	}

	return nil
}

var _ fs.NodeMkdirer = (*node)(nil)

func (n *node) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fuseNode fs.Node, err error) {
	defer func() {
		log.Printf("main: Mkdir produced %s, %+v", fuseNode, err)
	}()
	if n.readonly {
		return nil, fuse.ENOTSUP
	}
	if !n.dir {
		return nil, fuse.ENOTSUP
	}
	if err = n.loadChildrenIfEmpty(ctx); err != nil {
		log.Printf("Failed to load children of %q: %+v", n.id, err)
		return nil, err
	}
	g, err := n.gd.CreateNode(n.id, req.Name, true)
	if err != nil {
		log.Printf("Failed to create node %q: %v", req.Name, err)
		return nil, err
	}
	n.system.mu.Lock()
	defer n.system.mu.Unlock()
	created := n.insertNode(g)

	return created, nil
}

func (n *node) haveChildren() bool {
	n.cmu.Lock()
	loaded := n.children != nil
	n.cmu.Unlock()
	return loaded
}

func (n *node) findChild(name string) (*node, error) {
	if !n.haveChildren() {
		panic(fmt.Sprintf("findChild on %q called for %q before loadChildrenIfEmpty was called.  Unable to continue.", n.id, name))
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

func (n *node) loadChildrenIfEmpty(ctx context.Context) error {
	if n.haveChildren() {
		return nil
	}

	gs, err := n.gd.FetchChildren(ctx, n.id)
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

	return n.findChild(name)
}

var _ fs.NodeCreater = (*node)(nil)

func (n *node) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fuseNode fs.Node, h fs.Handle, err error) {
	defer func() {
		log.Printf("main: Create produced %s, %s, %#v", fuseNode, h, err)
	}()
	if n.readonly {
		return nil, nil, fuse.ENOTSUP
	}
	if !n.dir {
		return nil, nil, fuse.ENOTSUP
	}
	if err = n.loadChildrenIfEmpty(ctx); err != nil {
		log.Printf("Failed to load children of %q: %v", n.id, err)
		return nil, nil, err
	}
	dir := req.Mode&os.ModeDir != 0
	g, err := n.gd.CreateNode(n.id, req.Name, dir)
	if err != nil {
		log.Printf("Failed to create node %q: %v", req.Name, err)
		return nil, nil, err
	}
	n.system.mu.Lock()
	defer n.system.mu.Unlock()
	created := n.insertNode(g)

	resp.Node = fuse.NodeID(created.idx)
	created.Attr(ctx, &resp.Attr)

	handle, err := created.pf.Open(phantomfile.WriteOnly, phantomfile.NoFetch)
	if err != nil {
		log.Printf("Failed to open file for node %q: %v", created.id, err)
		return nil, nil, err
	}
	return created, handle, nil
}

var _ fs.NodeOpener = (*node)(nil)

func xlateAccessMode(flags fuse.OpenFlags) phantomfile.AccessMode {
	switch {
	case flags.IsReadOnly():
		return phantomfile.ReadOnly
	case flags.IsWriteOnly():
		return phantomfile.WriteOnly
	default:
		return phantomfile.ReadWrite
	}
}

func (n *node) Open(ctx context.Context, req *fuse.OpenRequest, res *fuse.OpenResponse) (handle fs.Handle, err error) {
	if n.dir {
		// send the caller to ReadDirAll
		return n, nil
	}
	if req.Flags&fuse.OpenExclusive != 0 {
		// Google drive doesn't support this concept (it is fine
		// having two files with the same name in the same folder), so
		// we don't either.
		log.Print("Open failing due to unsupported exclusive flag")
		return nil, fuse.ENOTSUP
	}

	am := xlateAccessMode(req.Flags)

	if am != phantomfile.ReadOnly && n.readonly {
		log.Print("Open: failing due to writeable request of readonly filesystem")
		return nil, fuse.EPERM
	}

	defer func() {
		if handle != nil && err != nil {
			h := handle.(fs.HandleReleaser)
			h.Release(ctx, &fuse.ReleaseRequest{})
		}
	}()
	// TODO(gina) fix up read/write handling
	switch {
	case am == phantomfile.ReadOnly:
		res.Flags |= fuse.OpenKeepCache
		return n.pf.Open(am, phantomfile.ProactiveFetch)
	case req.Flags&fuse.OpenTruncate != 0:
		handle, err = n.pf.Open(am, phantomfile.NoFetch)
		if err != nil {
			err = n.pf.Truncate(ctx, 0)
		}
		return handle, err
	default:
		return nil, fuse.Errno(syscall.EACCES)
	}
}

var _ fs.NodeRenamer = (*node)(nil)

func (n *node) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	if n.readonly {
		log.Print("Rename: failing because readonly")
		return fuse.ENOTSUP
	}
	if !n.dir {
		log.Print("Rename: failing because not a directory")
		return fuse.ENOTSUP
	}
	if err := n.loadChildrenIfEmpty(ctx); err != nil {
		log.Printf("Rename: load failed %v", err)
		return fuse.EIO
	}

	child, err := n.findChild(req.OldName)
	if child == nil {
		log.Printf("Rename: failed because unable to find %q in %q", req.OldName, n.id)
		return fuse.ENOENT
	}

	var oldParentID string
	var newParentID string
	if newDir != nil {
		newParent, ok := newDir.(*node)
		if !ok {
			log.Printf("*node newDir node isn't a *node, is a %T; can't handle.  returning EIO.", newDir)
			return fuse.EIO
		}
		oldParentID = n.id
		newParentID = newParent.id
		if oldParentID == newParentID {
			oldParentID = ""
			newParentID = ""
		}
	}
	log.Printf("Renaming %q with newName %q.  oldParentID=%q and newParentID=%q", child.id, req.NewName, oldParentID, newParentID)
	gnode, err := n.system.gd.Rename(ctx, child.id, req.NewName, oldParentID, newParentID)
	if err != nil {
		return err
	}
	n.system.mu.Lock()
	defer n.system.mu.Unlock()
	child.update(gnode)
	return nil
}

var _ fs.NodeRemover = (*node)(nil)

func (n *node) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	if n.readonly {
		log.Print("Rename: failing because readonly")
		return fuse.ENOTSUP
	}
	if !n.dir {
		log.Print("Rename: failing because not a directory")
		return fuse.ENOTSUP
	}
	if err := n.loadChildrenIfEmpty(ctx); err != nil {
		log.Printf("Rename: load failed %v", err)
		return fuse.EIO
	}

	child, err := n.findChild(req.Name)
	if child == nil {
		log.Printf("Remove: failed because unable to find %q in %q", req.Name, n.id)
		return fuse.ENOENT
	}

	err = n.system.gd.Trash(ctx, child.id)
	if err != nil {
		return err
	}
	n.system.mu.Lock()
	defer n.system.mu.Unlock()
	n.system.removeNode(child)
	return nil
}

var _ phantomfile.DownloaderUploader = (*node)(nil)

func (n *node) Download(ctx context.Context, f *os.File) error {
	return n.gd.Download(ctx, n.id, f)
}

func (n *node) Upload(ctx context.Context, f *os.File) error {
	return n.gd.Upload(ctx, n.id, f)
}

func (n *node) ID() string {
	return n.id
}

func (n *node) Name() string {
	return n.name
}

func (n *node) String() string {
	return fmt.Sprintf("%s/%s", n.id,
		n.name)
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
	a.Mode = modeReadOnly

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
