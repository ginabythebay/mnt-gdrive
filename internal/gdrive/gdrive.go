package gdrive

import (
	"fmt"
	"io/ioutil"
	"os/user"
	"path"

	"google.golang.org/api/drive/v3"

	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
)

// Gdrive corresponds to a google drive connection
type Gdrive struct {
	svc       *drive.Service
	pageToken string
}

// GetService returns a drive service, or an error.
func GetService() (*Gdrive, error) {
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
	config, err := google.ConfigFromJSON(b, drive.DriveReadonlyScope)
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

	return &Gdrive{svc, token}, nil
}
