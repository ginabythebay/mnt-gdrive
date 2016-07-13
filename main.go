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

	"google.golang.org/api/drive/v3"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	_ "bazil.org/fuse/fs/fstestutil"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"

	"golang.org/x/oauth2"
)

// TODO(gina) things to address
// . doing something with inodes will help perf?
// . better file modes
// . consider readonly mounting mode.  would affect flags we return in attributes,
//   whether we allow opens for writes, and whether we ask google for full access.
// . caching?
// . prefetch of extra file attributes during ReadDirAll?

const PageSize = 1000

// TODO(gina) do something realz here
const MODE_FILE = 0444
const MODE_DIR = os.ModeDir | 0555

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

	srv, err := getDriveService()
	if err != nil {
		log.Fatal(err)
	}

	c, err := fuse.Mount(
		mountpoint,
		fuse.FSName("mntgdrive"),
		fuse.Subtype("mntgrdrivefs"),
		fuse.LocalVolume(),
		fuse.VolumeName("GDrive!"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	log.Print("Entering Serve")

	err = fs.Serve(c, System{srv})
	if err != nil {
		log.Fatal(err)
	}

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		log.Fatal(err)
	}
}

// FS implements the hello world file system.
type System struct {
	srv *drive.Service
}

func (s System) Root() (fs.Node, error) {
	return Dir{s.srv, "root"}, nil
}

// Dir implements both Node and Handle for the root directory.
type Dir struct {
	srv *drive.Service
	id  string
}

func (d Dir) ChildQuery(nextPageToken string) *drive.FilesListCall {
	result := d.srv.Files.List().PageSize(PageSize).
		Fields("nextPageToken, files(id, name)").
		Q(fmt.Sprintf("'%s' in parents", d.id))
	if nextPageToken != "" {
		result = result.PageToken(nextPageToken)
	}
	return result
}

func (Dir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = MODE_DIR
	return nil
}

func (Dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name == "hello" {
		return File{}, nil
	}
	return nil, fuse.ENOENT
}

func (d Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	result := make([]fuse.Dirent, 0)
	r, err := d.ChildQuery("").Do()
	if err != nil {
		log.Print("Unable to retrieve files.", err)
		return nil, fuse.ENODATA
	}

	for len(r.Files) > 0 {
		for _, f := range r.Files {
			name := strings.Replace(f.Name, "/", "_", -1)
			log.Print(name)
			result = append(result,
				fuse.Dirent{Name: name, Type: fuse.DT_File})
		}

		if r.NextPageToken == "" {
			break
		}
		r, err = d.ChildQuery(r.NextPageToken).Do()
		if err != nil {
			log.Print("Unable to retrieve files.", err)
			return nil, fuse.ENODATA
		}
	}

	return result, nil
}

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
