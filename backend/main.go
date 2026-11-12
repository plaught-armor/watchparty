// Command backend is the WatchParty server: it serves the built web app,
// a small HTTP API, and one WebSocket per viewer. All state lives in memory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxCreateRoomBody  = 1 << 20
	maxPreloadPlaylist = 25
	roomNameAttempts   = 5
)

type Config struct {
	Addr             string
	BuildDir         string
	WordsDir         string
	YoutubeKey       string
	OpenSubtitlesKey string
	MaxRooms         int
	MaxConns         int
	// RoomCapacity limits viewers per room; 0 means no limit.
	RoomCapacity int
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Fatalf("%s must be a non-negative integer, got %q", name, v)
	}
	return n
}

func loadConfig() Config {
	return Config{
		Addr:             envOr("HOST", "0.0.0.0") + ":" + envOr("PORT", "8080"),
		BuildDir:         envOr("BUILD_DIRECTORY", "build"),
		WordsDir:         envOr("WORDS_DIRECTORY", "words"),
		YoutubeKey:       os.Getenv("YOUTUBE_API_KEY"),
		OpenSubtitlesKey: os.Getenv("OPENSUBTITLES_KEY"),
		MaxRooms:         envInt("MAX_ROOMS", 2000),
		MaxConns:         envInt("MAX_CONNS", 5000),
		RoomCapacity:     envInt("ROOM_CAPACITY", 0),
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write json: %v", err)
	}
}

// writeText sends a short body, as plain text unless the caller set a type.
func writeText(w http.ResponseWriter, text string) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	if _, err := w.Write([]byte(text)); err != nil {
		return // client went away
	}
}

// cors lets a web app hosted on another origin use the API. Nothing here is
// authenticated by cookies, so any origin is allowed.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if req.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// createRoom answers POST /createRoom with a fresh room name, optionally
// preloaded with a video or playlist.
func (h *Hub) createRoom(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Video    string   `json:"video"`
		Playlist []string `json:"playlist"`
	}
	err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxCreateRoomBody)).Decode(&body)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	room, err := h.newNamedRoom()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if body.Video != "" && len(body.Video) <= maxPlaylistURL {
		room.mu.Lock()
		room.video = body.Video
		// Wait for the first viewer to press play.
		room.paused = true
		room.mu.Unlock()
	}
	if len(body.Playlist) > 0 && len(body.Playlist) <= maxPreloadPlaylist {
		// Each item may need a YouTube lookup, so do not hold up the response.
		go func() {
			for _, link := range body.Playlist {
				room.playlistAdd(nil, h.yt, link)
			}
		}()
	}
	writeJSON(w, map[string]string{"name": "/" + room.id})
}

// newNamedRoom creates a room under a random unused name.
func (h *Hub) newNamedRoom() (*Room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for range roomNameAttempts {
		name := h.names.roomName()
		if h.rooms[name] == nil {
			return h.room(name)
		}
	}
	return nil, errors.New("could not find a free room name")
}

func (h *Hub) youtubeSearch(w http.ResponseWriter, req *http.Request) {
	videos, err := h.yt.search(req.URL.Query().Get("q"))
	if err != nil {
		log.Print(err)
	}
	writeJSON(w, videos)
}

func (h *Hub) youtubePlaylist(w http.ResponseWriter, req *http.Request) {
	videos, err := h.yt.playlist(req.PathValue("id"))
	if err != nil {
		log.Print(err)
	}
	writeJSON(w, videos)
}

// Features of the hosted service that need accounts, a database or payments.
var unsupportedRoutes = []string{
	"/stats", "/timeSeries", "/health/{id}", "/proxy", "/checkoutSub", "/subSuccess",
	"/manageSub", "/updateEmail", "/deleteAccount", "/listRooms", "/deleteRoom",
	"/linkAccount", "/kv", "/generateMediasoupToken",
}

func notSupported(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotImplemented)
	writeText(w, `{"error":"not supported"}`)
}

// staticHandler serves the built web app. Unknown paths get index.html so
// client-side routes such as /watch/room-name load the app.
func staticHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	root := os.DirFS(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+req.URL.Path), "/")
		isAsset := strings.HasPrefix(name, "assets/")
		if info, err := fs.Stat(root, name); name != "" && err == nil && !info.IsDir() {
			if isAsset {
				// Vite puts a content hash in every asset file name.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, req)
			return
		}
		if isAsset {
			// A missing script must fail, not load the HTML page as JavaScript.
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, req, filepath.Join(dir, "index.html"))
	})
}

func (h *Hub) routes(subs *subtitleAPI) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/{room}", h.serveWS)
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) { writeText(w, "pong") })
	mux.HandleFunc("POST /createRoom", h.createRoom)
	mux.HandleFunc("GET /generateName", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, h.names.userName())
	})
	mux.HandleFunc("GET /youtube", h.youtubeSearch)
	mux.HandleFunc("GET /youtubePlaylist/{id}", h.youtubePlaylist)
	mux.HandleFunc("GET /metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]bool{"isSubscriber": false, "isFreePoolFull": false, "beta": false})
	})
	// No vanity names or saved room settings without a database.
	null := func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, nil) }
	mux.HandleFunc("GET /resolveRoom/{vanity}", null)
	mux.HandleFunc("GET /roomData/{id}", null)
	mux.HandleFunc("GET /resolveShard/{id}", func(w http.ResponseWriter, _ *http.Request) { writeText(w, "") })
	mux.HandleFunc("POST /subtitle", subs.upload)
	mux.HandleFunc("GET /subtitle/{hash}", subs.download)
	mux.HandleFunc("GET /searchSubtitles", subs.search)
	mux.HandleFunc("GET /downloadSubtitles", subs.fetch)
	for _, route := range unsupportedRoutes {
		mux.HandleFunc(route, notSupported)
	}
	mux.Handle("/", staticHandler(h.cfg.BuildDir))
	return cors(mux)
}

func main() {
	cfg := loadConfig()
	names, err := newNameGenerator(cfg.WordsDir)
	if err != nil {
		log.Fatalf("load word lists: %v", err)
	}
	hub := newHub(cfg, newYoutubeClient(cfg.YoutubeKey), names)
	store := newSubtitleStore()
	subs := &subtitleAPI{
		store: store,
		key:   cfg.OpenSubtitlesKey,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go hub.run(ctx.Done(), store)

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           hub.routes(subs),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("listening on %s", cfg.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}
