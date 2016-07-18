# mnt-gdrive

Allows you to mount google drive via FUSE.  Readonly for now.  Probably has lots of wierd dangerous bugs.

It has only been tested on Linux.  If you want local access to your google drive files on a Mac,  I suggest using the [google provided solution](https://tools.google.com/dlpage/drive).

Right now, it excludes all files not owned by you.  I expect to make that a command line option in the future.

## Getting Started

```
go get -u github.com/ginabythebay/mnt-gdrive
go install github.com/ginabythebay/mnt-gdrive
```

Follow the directions under 'Step 1: Turn on the Drive API' found on this [page](https://developers.google.com/drive/v3/web/quickstart/go) and put the `client_secret.json` file into the `~/.config/mnt-gdrive` directory.

Pick a mount point.  I'll assume `/tmp/mnt` in the example below.

```
mnt-gdrive /tmp/mnt
```

That is it.  You should be able to do normal read-only things, like `ls` or `find` or `cat`.

You will see various things appearing on stdout as it runs.

## Design

### node

The central data structure is `node` which corresponds to a (Google
Drive File)[https://developers.google.com/drive/v3/reference/files].
This can be either a directory or a file, as far as the kernel is
concerned.

Each node has a reference to its parent(s) as well as to children.  If
we haven't loaded children yet for a node, then that field will be
nil.

### Locking

Each load has two mutexes.  `mu` guards metadata like `size` and
`parents` while `cmu` guards `children`.  During some updates, we will
hold the `mu` lock for child node and then hold-and-release `cmu`
locks for the associated parents.

### Memory Management

We load nodes from google drive on demand.  We keep them up to date by
polling the (Google Drive Change
API)[https://developers.google.com/drive/v3/reference/changes].  This
asynchronous polling means that we never have to wait on network
activity to return information about a node once it has been loaded
but also means that we have constant background network activity even
when nothing is accessing the file system.  You probably don't want to
use this file system over a metered connection.  I'll probably add an
option to have this file system unmount itself after it has been idle
for a while.

We currently never expire old nodes, but we could easily start
tracking last-access time and after a node hasn't been used in a
while, we could discard it.  Currently we always assume that nodes
have parent lists that reflect what is in google drive, so we would
need to discard from the bottom up.  I suppose if we found that we had
discarded all nodes we could potentially pause the background polling.
Would need to think about how to handle the root node in that case.
Currently we assume we have a valid root node from the beginning.

## Tricks

You can cat a magic invisible `.dump` file at the root of the file
system that will show you a dump of the node tree.

I am toying with the idea of having a similar magic file you can write
to do dynamically change e.g. logging behavior.

## Links

  * (planning.org)[planning.org] contains a semi-truthful plan
  * (Bazil FUSE)[https://bazil.org/fuse/] is a library we leverage
    heavily
  * (Google Drive
    API)[https://developers.google.com/drive/v3/web/about-sdk] is how
    we interact with Google Drive
