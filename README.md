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

## Tricks

.dump file
