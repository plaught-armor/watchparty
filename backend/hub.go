package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sessionTTL  = 7 * 24 * time.Hour
	maxSessions = 50000
)

type session struct {
	id   string
	seen time.Time
}

// Hub owns every room. Lock order is always hub.mu, then room.mu.
type Hub struct {
	cfg   Config
	yt    *youtubeClient
	names *nameGenerator

	conns atomic.Int64

	mu    sync.Mutex
	rooms map[string]*Room
	// sessions binds a clientID to the secret session ID that first used it,
	// so one user cannot post as another by copying the public clientID.
	sessions map[string]session
}

func newHub(cfg Config, yt *youtubeClient, names *nameGenerator) *Hub {
	return &Hub{
		cfg:      cfg,
		yt:       yt,
		names:    names,
		rooms:    map[string]*Room{},
		sessions: map[string]session{},
	}
}

func (h *Hub) acquireConn() bool {
	if h.conns.Add(1) > int64(h.cfg.MaxConns) {
		h.conns.Add(-1)
		return false
	}
	return true
}

func (h *Hub) releaseConn() {
	h.conns.Add(-1)
}

// claimSession reports whether sessionID may act as clientID.
func (h *Hub) claimSession(clientID, sessionID string, now time.Time) bool {
	if len(sessionID) < 16 || len(sessionID) > 200 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	known, ok := h.sessions[clientID]
	if ok && now.Sub(known.seen) < sessionTTL && known.id != sessionID {
		return false
	}
	// At the cap new clients still connect, they just are not protected.
	if ok || len(h.sessions) < maxSessions {
		h.sessions[clientID] = session{id: sessionID, seen: now}
	}
	return true
}

var (
	errServerFull = errors.New("This server has too many rooms right now")
	errRoomFull   = errors.New("This room is full")
)

// room returns the room with this ID, creating it when absent. Rooms live in
// memory only, so a link must keep working after the server restarts.
// Caller holds h.mu.
func (h *Hub) room(id string) (*Room, error) {
	if r := h.rooms[id]; r != nil {
		return r, nil
	}
	if len(h.rooms) >= h.cfg.MaxRooms {
		return nil, errServerFull
	}
	r := newRoom(id)
	h.rooms[id] = r
	return r, nil
}

// joinRoom adds c to the room. Both locks are held so the janitor cannot
// reap the room between lookup and join.
func (h *Hub) joinRoom(id string, c *Conn) (*Room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, err := h.room(id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, rejoining := r.conns[c.clientID]
	if !rejoining && h.cfg.RoomCapacity > 0 && len(r.conns) >= h.cfg.RoomCapacity {
		return nil, errRoomFull
	}
	r.join(c)
	return r, nil
}

func (h *Hub) snapshot() []*Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	return rooms
}

// reap drops idle empty rooms and expired sessions.
func (h *Hub) reap(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, r := range h.rooms {
		if r.reapable(now) {
			delete(h.rooms, id)
		}
	}
	for clientID, s := range h.sessions {
		if now.Sub(s.seen) >= sessionTTL {
			delete(h.sessions, clientID)
		}
	}
}

// run drives the periodic work: viewer timestamps every second, cleanup every
// minute. It returns when stop is closed.
func (h *Hub) run(stop <-chan struct{}, subs *subtitleStore) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	cleanup := time.NewTicker(time.Minute)
	defer cleanup.Stop()
	for {
		select {
		case now := <-tick.C:
			for _, r := range h.snapshot() {
				r.tick(now)
			}
		case now := <-cleanup.C:
			h.reap(now)
			subs.prune(now)
		case <-stop:
			return
		}
	}
}
