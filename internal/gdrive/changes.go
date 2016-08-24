package gdrive

import (
	"fmt"
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
func getStartPageToken(service *drive.Service) (string, error) {
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
func (gd *Gdrive) ProcessChanges(changeHandler func(*Change, *ChangeStats)) (ChangeStats, error) {
	cs := ChangeStats{}
	gd.pageMu.Lock()
	defer gd.pageMu.Unlock()
	token := gd.pageToken
	for token != "" {
		// TODO(gina) see if we can reduce notification spam.  Right
		// now we are getting notified every time the view time for
		// something gets updated and that isn't useful.  Maybe we can
		// exclude that field and get fewer notifications.
		cl, err := gd.svc.Changes.List(token).
			IncludeRemoved(true).
			RestrictToMyDrive(true).
			Fields(changeFields).
			Do()
		if err != nil {
			log.Printf("Error fetching changes: %v", err)
			return cs, err
		}
		for _, gChange := range cl.Changes {
			var n *Node
			if gChange.File != nil {
				n, err = newNode(gChange.FileId, gChange.File)
				if err != nil {
					log.Printf("Error converting changes %#v: %v", gChange, err)
					return cs, err
				}
				ch := &Change{n.ID, gChange.Removed, n}
				changeHandler(ch, &cs)
			}
		}
		if cl.NewStartPageToken != "" {
			gd.pageToken = cl.NewStartPageToken
		}
		token = cl.NextPageToken
	}
	return cs, nil
}

// ChangeStats totals up what happened
type ChangeStats struct {
	// Number of changes we applied
	Changed uint32
	// Number of changes we ignored
	Ignored uint32
}

func (cs *ChangeStats) String() string {
	return fmt.Sprintf("Processed %d changes and ignored %d changes", cs.Changed, cs.Ignored)
}

// FetchedChanges returns true if any changes were fetched
func (cs *ChangeStats) FetchedChanges() bool {
	return cs.Changed > 0 || cs.Ignored > 0
}
