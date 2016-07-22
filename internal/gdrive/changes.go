package gdrive

import (
	"log"

	"google.golang.org/api/drive/v3"
)

// Change represents a change to a node
type Change struct {
	ID      string
	Removed bool

	// Node: nil if the file was removed.
	Node *Node
}

// GetStartPageToken fetches the start page token to use for changes.
func GetStartPageToken(service *drive.Service) (string, error) {
	token, err := service.Changes.GetStartPageToken().Do()
	if err != nil {
		return "", err
	}

	return token.StartPageToken, nil
}

// ProcessChanges processes one set of changes, starting at the place
// determined by pageToken, until there are no more changes available.
// If there were changes, processed, then pageToken will be updated so
// the next time this is called, we will start from that point.  To
// start, set pageToken to a value returned by GetStartPageToken,
// above.  Each change will be passed one at a time to the
// changeHandler, which can return a counter that will be summed and
// the sum will be the returned by the ProccessChange function.
func ProcessChanges(service *drive.Service, pageToken *string, changeHandler func(*Change) uint32) (uint32, error) {
	token := *pageToken
	sum := uint32(0)
	for token != "" {
		// TODO(gina) see if we can reduce notification spam.  Right
		// now we are getting notified every time the view time for
		// something gets updated and that isn't useful.  Maybe we can
		// exclude that field and get fewer notifications.
		cl, err := service.Changes.List(token).
			IncludeRemoved(true).
			RestrictToMyDrive(true).
			Fields(changeFields).
			Do()
		if err != nil {
			log.Printf("Error fetching changes: %v", err)
			return sum, err
		}
		for _, gChange := range cl.Changes {
			var n *Node
			if gChange.File != nil {
				n, err = newNode(gChange.FileId, gChange.File)
				if err != nil {
					log.Printf("Error converting changes %#v: %v", gChange, err)
					return sum, err
				}
			}
			ch := &Change{n.ID, gChange.Removed, n}
			sum += changeHandler(ch)
		}
		if cl.NewStartPageToken != "" {
			*pageToken = cl.NewStartPageToken
		}
		token = cl.NextPageToken
	}
	return sum, nil
}
