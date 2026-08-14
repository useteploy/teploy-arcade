package arcade

import (
	"sync"
	"sync/atomic"
	"time"
)

// Hub is a room-per-server console fanout.
//
// Two things it does that neutronrealtime's Hub does not, both of which the
// console UI depends on:
//
//   - Every Conn counts its own dropped messages (NEUTRON-ISSUES NI-008). The
//     browser cannot count what it never received, so an honest "N lines
//     dropped" notice has to come from here.
//   - Every line carries a monotonic per-room sequence number, so a client can
//     localise a gap, dedupe a replay, and reconcile after a reconnect.
//
// Inbound client messages are NOT fanned out to peers (NI-002): a console
// command goes to the game server's stdin, never to other viewers' sockets.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]*Room
}

type Room struct {
	ID string
	// dead marks a room whose server was deleted. The entry is kept rather than
	// removed so a late Publish from a leaked runner goroutine cannot rebuild a
	// 500-line ring buffer nothing will ever drain. A tombstone costs a few
	// words; a resurrected room costs 500 lines for the life of the process.
	dead  bool
	mu    sync.Mutex
	seq   int64
	ring  []Line // last ringSize lines, for replay on connect
	conns map[*Conn]struct{}
}

const ringSize = 500

// Conn is one viewer socket.
type Conn struct {
	Send    chan []byte
	dropped atomic.Int64

	// sendMu makes "is it closed?" and "send" one atomic step. An atomic flag
	// checked before the send is NOT enough: Close can run in the gap between
	// the check and the send, and sending on a closed channel is an
	// unrecoverable panic that takes the whole panel with it. The upstream
	// neutronrealtime Hub guards both with the same mutex for exactly this
	// reason; replacing it with a flag reintroduced the bug it prevents.
	sendMu sync.Mutex
	closed bool
}

func NewHub() *Hub { return &Hub{rooms: map[string]*Room{}} }

func (h *Hub) room(id string) *Room {
	h.mu.RLock()
	r, ok := h.rooms[id]
	h.mu.RUnlock()
	if ok {
		return r
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rooms[id]; ok {
		return r
	}
	r = &Room{ID: id, conns: map[*Conn]struct{}{}}
	h.rooms[id] = r
	return r
}

// lookup finds a room without creating one. room() is get-or-create, which is
// what Join and Publish need and what everything else must avoid: a Leave that
// lands after DropRoom, or a status push for a server that has just been
// deleted, would otherwise materialise a room nobody will ever read again - and
// rooms are only ever removed by DropRoom, so it would live for the life of the
// process holding a 500-line ring buffer.
func (h *Hub) lookup(id string) (*Room, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.rooms[id]
	return r, ok
}

func NewConn(buf int) *Conn { return &Conn{Send: make(chan []byte, buf)} }

// Dropped reports how many messages this connection has shed under
// backpressure. Monotonic; the reader diffs it.
func (c *Conn) Dropped() int64 { return c.dropped.Load() }

func (c *Conn) Close() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.Send)
}

// trySend never blocks the broadcaster. A slow viewer sheds lines rather than
// stalling every other viewer or the log reader behind it.
func (c *Conn) trySend(msg []byte) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.Send <- msg:
	default:
		c.dropped.Add(1)
	}
}

// Join registers a viewer and returns the replay buffer plus the sequence the
// client is caught up to. buffer_capacity is reported so the UI's "restored the
// last N lines" is a fact rather than a hardcoded number.
func (h *Hub) Join(roomID string, c *Conn) (replay []Line, seq int64, capacity int) {
	r := h.room(roomID)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[c] = struct{}{}
	replay = make([]Line, len(r.ring))
	copy(replay, r.ring)
	return replay, r.seq, ringSize
}

func (h *Hub) Leave(roomID string, c *Conn) {
	// The room is often already gone: DropRoom closes every socket, and each
	// reader then runs its deferred Leave. Closing the conn is still required -
	// Close is idempotent, and a Leave without a preceding DropRoom is the
	// normal case.
	if r, ok := h.lookup(roomID); ok {
		r.mu.Lock()
		delete(r.conns, c)
		r.mu.Unlock()
	}
	c.Close()
}

// Publish stamps a line with the next sequence number, appends it to the ring
// buffer and fans it out. Returns the stamped line.
func (h *Hub) Publish(roomID string, l Line) Line {
	// Get-or-create is correct here: the ring fills before anyone connects,
	// which is what makes replay work for a viewer who opens the console after
	// the server booted. A deleted server's room is still in the map as a
	// tombstone, so this returns that rather than building a fresh one.
	r := h.room(roomID)

	l.Text = truncateLine(l.Text)

	r.mu.Lock()
	// Checked here, under the lock that DropRoom writes it under, rather than
	// in a separate pre-flight pass: r.mu is the only lock that orders dead
	// against the ring append, and the check is free inside a section Publish
	// takes anyway.
	if r.dead {
		r.mu.Unlock()
		return l
	}
	r.seq++
	l.Seq = r.seq
	if l.TS == "" {
		l.TS = time.Now().Format("15:04:05")
	}
	if l.Level == "" {
		l.Level = "info"
	}
	if l.Source == "" {
		l.Source = "server"
	}
	r.ring = append(r.ring, l)
	if len(r.ring) > ringSize {
		r.ring = r.ring[len(r.ring)-ringSize:]
	}
	conns := make([]*Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()

	msg := mustJSON(map[string]any{"t": "line", "line": l})
	for _, c := range conns {
		c.trySend(msg)
	}
	return l
}

// PublishRaw fans out a non-line control message (status, players, acks).
// Nothing is buffered, so with no room there is nobody to tell.
func (h *Hub) PublishRaw(roomID string, v any) {
	r, ok := h.lookup(roomID)
	if !ok {
		return
	}
	r.mu.Lock()
	conns := make([]*Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()

	msg := mustJSON(v)
	for _, c := range conns {
		c.trySend(msg)
	}
}

// Tail returns the most recent n buffered lines for a room. Used by the MCP
// console_tail tool, which has no socket to replay onto.
func (h *Hub) Tail(roomID string, n int) []Line {
	r, ok := h.lookup(roomID)
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.ring) {
		n = len(r.ring)
	}
	out := make([]Line, n)
	copy(out, r.ring[len(r.ring)-n:])
	return out
}

// DropRoom releases a room and its ring buffer. Called when a server is
// deleted - without it every deleted server leaks 500 buffered lines for the
// life of the process.
func (h *Hub) DropRoom(roomID string) {
	h.mu.Lock()
	r, ok := h.rooms[roomID]
	h.mu.Unlock()
	if !ok {
		return
	}
	r.mu.Lock()
	for c := range r.conns {
		c.Close()
	}
	r.conns = map[*Conn]struct{}{}
	r.ring = nil // the memory that mattered
	r.dead = true
	r.mu.Unlock()
}

// Viewers reports how many sockets are attached to a room.
func (h *Hub) Viewers(roomID string) int {
	r, ok := h.lookup(roomID)
	if !ok {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}
