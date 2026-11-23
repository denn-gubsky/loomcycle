package lifecycle

import (
	"fmt"
	"testing"
	"time"
)

// One workspace that takes many checkouts drops only its own oldest
// records: another workspace's record, of another run or tenant, survives.
func TestCheckouts_AWorkspaceFillingItsCapKeepsAnothersRecord(t *testing.T) {
	var c checkouts
	now := time.Unix(1000, 0)
	c.remember("ws_b", "b1", []byte("b"), now)
	for i := range 1000 {
		c.remember("ws_a", fmt.Sprintf("a%d", i), []byte("a"), now.Add(time.Duration(i+1)*time.Second))
	}
	if _, ok := c.lookup("ws_b", "b1"); !ok {
		t.Fatal("workspace A's checkouts evicted workspace B's record")
	}
	if _, ok := c.lookup("ws_a", "a999"); !ok {
		t.Error("workspace A's newest record is gone")
	}
	if _, ok := c.lookup("ws_a", "a0"); ok {
		t.Error("workspace A keeps more records than its cap")
	}
}

// A released workspace's records go with it.
func TestCheckouts_AReleasedWorkspacesRecordsAreDropped(t *testing.T) {
	var c checkouts
	c.remember("ws_a", "a1", []byte("a"), time.Unix(1000, 0))
	c.forget("ws_a")
	if _, ok := c.lookup("ws_a", "a1"); ok {
		t.Fatal("a released workspace's record survived")
	}
}

// A runtime that holds no record (another replica, or after a restart)
// finds none: the caller refuses the commit.
func TestCheckouts_ARecordThisProcessNeverKeptIsNotFound(t *testing.T) {
	var c checkouts
	if _, ok := c.lookup("ws_a", "a1"); ok {
		t.Fatal("a record never kept was found")
	}
}
