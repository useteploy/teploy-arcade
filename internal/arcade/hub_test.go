package arcade

import "testing"

func TestDropRoomBeforeFirstPublishTombstonesTheRoom(t *testing.T) {
	h := NewHub()
	h.DropRoom("never-existed")
	// A later publish to the deleted ID must not resurrect the room.
	l := h.Publish("never-existed", Line{Text: "hello"})
	if r, ok := h.lookup("never-existed"); !ok || !r.dead {
		t.Fatal("DropRoom before first Publish must leave a dead tombstone, not a live room")
	}
	_ = l
}

func TestJoinAfterDropRoomGainsNoViewers(t *testing.T) {
	h := NewHub()
	h.Publish("gone", Line{Text: "x"})
	h.DropRoom("gone")
	c := NewConn(4)
	defer c.Close()
	replay, _, _ := h.Join("gone", c)
	if n := h.Viewers("gone"); n != 0 {
		t.Fatalf("dead room must not gain viewers, got %d", n)
	}
	if len(replay) != 0 {
		t.Fatalf("dead room must not replay buffered lines, got %d", len(replay))
	}
}
