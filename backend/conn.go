package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	sendQueueSize = 256
	readLimit     = 256 << 10
	helloTimeout  = 5 * time.Second
	writeTimeout  = 10 * time.Second
	pingInterval  = 25 * time.Second
	// Close code telling the client it was removed on purpose and must not reconnect.
	closeKicked = websocket.StatusCode(4000)
)

var (
	roomIDPattern   = regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`)
	clientIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// Conn is one client's WebSocket. Frames are queued on send and written by a
// single goroutine, so room code never blocks on a slow network peer.
type Conn struct {
	clientID string
	ws       *websocket.Conn
	send     chan []byte

	stopOnce sync.Once
	stopped  chan struct{}
	stopCode websocket.StatusCode
}

func newConn(ws *websocket.Conn, clientID string) *Conn {
	return &Conn{
		clientID: clientID,
		ws:       ws,
		send:     make(chan []byte, sendQueueSize),
		stopped:  make(chan struct{}),
	}
}

// push queues a frame without blocking. A client that cannot keep up is
// disconnected; it will reconnect and receive fresh room state.
func (c *Conn) push(frame []byte) {
	select {
	case c.send <- frame:
	default:
		c.stop(websocket.StatusPolicyViolation)
	}
}

func (c *Conn) emit(event string, data any) {
	if frame := encode(event, data); frame != nil {
		c.push(frame)
	}
}

// stop asks the writer to flush queued frames and close with code. Safe to
// call from any goroutine, any number of times; the first code wins.
func (c *Conn) stop(code websocket.StatusCode) {
	c.stopOnce.Do(func() {
		c.stopCode = code
		close(c.stopped)
	})
}

func (c *Conn) kick() {
	c.stop(closeKicked)
}

func (c *Conn) write(ctx context.Context, frame []byte) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, frame)
}

func (c *Conn) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.ws.Ping(ctx)
}

// writeLoop owns all writes to the socket. It always ends by closing the
// socket, which also unblocks the read loop.
func (c *Conn) writeLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		var err error
		select {
		case frame := <-c.send:
			err = c.write(ctx, frame)
		case <-ticker.C:
			err = c.ping(ctx)
		case <-c.stopped:
			c.flushAndClose(ctx)
			return
		}
		if err != nil {
			// Peer is gone or stalled; nothing useful to report to it.
			_ = c.ws.CloseNow()
			return
		}
	}
}

// flushAndClose writes whatever is still queued (for example the "kicked"
// notice), then performs the close handshake.
func (c *Conn) flushAndClose(ctx context.Context) {
	for range sendQueueSize {
		select {
		case frame := <-c.send:
			if err := c.write(ctx, frame); err != nil {
				_ = c.ws.CloseNow()
				return
			}
			continue
		default:
		}
		break
	}
	// A failed close handshake only means the peer already went away.
	_ = c.ws.Close(c.stopCode, "")
}

type hello struct {
	ClientID  string `json:"clientId"`
	SessionID string `json:"sessionId"`
}

func readHello(ctx context.Context, ws *websocket.Conn) (hello, error) {
	ctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	var h hello
	_, raw, err := ws.Read(ctx)
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, err
	}
	if !clientIDPattern.MatchString(h.ClientID) {
		return h, errors.New("Invalid client ID")
	}
	return h, nil
}

// reject tells the client why it cannot join, then closes. The client does not
// retry after a connect_error.
func reject(ctx context.Context, ws *websocket.Conn, message string) {
	frame := encode("connect_error", map[string]string{"message": message})
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, frame); err != nil {
		_ = ws.CloseNow()
		return
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// serveWS handles GET /ws/{room}: handshake, hello, then the read loop until
// the client leaves.
func (h *Hub) serveWS(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("room")
	if !roomIDPattern.MatchString(roomID) {
		http.Error(w, "invalid room", http.StatusBadRequest)
		return
	}
	if !h.acquireConn() {
		http.Error(w, "server is full", http.StatusServiceUnavailable)
		return
	}
	defer h.releaseConn()
	// Auth is not cookie based, so cross-origin pages gain nothing by connecting.
	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("ws accept: %v", err)
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(readLimit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hi, err := readHello(ctx, ws)
	if err != nil {
		reject(ctx, ws, "Invalid client ID")
		return
	}
	if !h.claimSession(hi.ClientID, hi.SessionID, time.Now()) {
		reject(ctx, ws, "This client ID is in use by another session")
		return
	}
	c := newConn(ws, hi.ClientID)
	room, err := h.joinRoom(roomID, c)
	if err != nil {
		reject(ctx, ws, err.Error())
		return
	}
	go c.writeLoop(ctx)
	h.readLoop(ctx, room, c)
	room.leave(c)
	c.stop(websocket.StatusNormalClosure)
}

func (h *Hub) readLoop(ctx context.Context, room *Room, c *Conn) {
	for {
		_, raw, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		room.dispatch(c, h.yt, raw)
	}
}
