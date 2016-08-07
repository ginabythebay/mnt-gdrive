package gdrive

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/user"
	"path"
	"sync"

	"google.golang.org/api/drive/v3"

	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
)

// DriveLike is something that can perform google-drive like actions.
type DriveLike interface {
	FetchNode(id string) (n *Node, err error)
	CreateNode(parentID string, name string, dir bool) (n *Node, err error)
	FetchChildren(ctx context.Context, id string) (children []*Node, err error)
	Download(ctx context.Context, id string, f *os.File) error
	Upload(ctx context.Context, id string, f *os.File) error
	ProcessChanges(changeHandler func(*Change, *ChangeStats)) (ChangeStats, error)
}

// Gdrive corresponds to a google drive connection
type Gdrive struct {
	svc *drive.Service

	pageMu    sync.Mutex
	pageToken string
}

// GetService returns a drive service, or an error.
func GetService(readonly bool) (DriveLike, error) {
	ctx := context.Background()

	usr, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("Unable to determine home directory: %v", err)
	}

	secretFile := path.Join(usr.HomeDir, ".config", "mnt-gdrive", "client_secret.json")
	b, err := ioutil.ReadFile(secretFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to read client secret file: %v", err)
	}

	// If modifying these scopes, delete your previously saved credentials
	// at ~/.credentials/drive-go-quickstart.json
	var scope string
	if readonly {
		scope = drive.DriveReadonlyScope
	} else {
		scope = drive.DriveScope
	}

	config, err := google.ConfigFromJSON(b, scope)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse client secret file to config: %v", err)
	}
	client := getClient(ctx, config)

	svc, err := drive.New(client)
	if err != nil {
		return nil, err
	}
	token, err := getStartPageToken(svc)
	if err != nil {
		return nil, err
	}

	return &Gdrive{svc: svc, pageToken: token}, nil
}
