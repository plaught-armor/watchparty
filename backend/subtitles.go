package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxSubtitleBytes   = 1 << 20
	subtitleTTL        = 24 * time.Hour
	subtitleStoreBytes = 32 << 20
	openSubtitlesAPI   = "https://api.opensubtitles.com/api/v1/"
)

var subtitleHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type subtitle struct {
	gz    []byte
	added time.Time
}

// subtitleStore keeps uploaded subtitle files in memory, gzipped, for a day.
type subtitleStore struct {
	mu    sync.Mutex
	files map[string]subtitle
	bytes int
}

func newSubtitleStore() *subtitleStore {
	return &subtitleStore{files: map[string]subtitle{}}
}

func gzipBytes(text []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(text); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// put stores text and returns its content hash, which is its lookup key.
func (s *subtitleStore) put(text []byte, now time.Time) (string, error) {
	gz, err := gzipBytes(text)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(text)
	hash := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.files[hash]; ok {
		s.bytes -= len(old.gz)
	}
	s.files[hash] = subtitle{gz: gz, added: now}
	s.bytes += len(gz)
	s.evict(hash)
	return hash, nil
}

// evict removes the oldest files until the store fits its budget, never
// touching keep. Caller holds s.mu.
func (s *subtitleStore) evict(keep string) {
	for s.bytes > subtitleStoreBytes && len(s.files) > 1 {
		oldest := ""
		for hash, f := range s.files {
			if hash != keep && (oldest == "" || f.added.Before(s.files[oldest].added)) {
				oldest = hash
			}
		}
		s.bytes -= len(s.files[oldest].gz)
		delete(s.files, oldest)
	}
}

func (s *subtitleStore) get(hash string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[hash]
	return f.gz, ok
}

func (s *subtitleStore) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, f := range s.files {
		if now.Sub(f.added) > subtitleTTL {
			s.bytes -= len(f.gz)
			delete(s.files, hash)
		}
	}
}

// subtitleAPI serves subtitle upload/download and proxies OpenSubtitles.
type subtitleAPI struct {
	store *subtitleStore
	key   string
	http  *http.Client
}

func (a *subtitleAPI) upload(w http.ResponseWriter, req *http.Request) {
	text, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxSubtitleBytes))
	if err != nil || len(text) == 0 {
		http.Error(w, "subtitle must be between 1 byte and 1 MB", http.StatusBadRequest)
		return
	}
	hash, err := a.store.put(text, time.Now())
	if err != nil {
		http.Error(w, "could not store subtitle", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"hash": hash})
}

func (a *subtitleAPI) download(w http.ResponseWriter, req *http.Request) {
	hash := req.PathValue("hash")
	gz, ok := a.store.get(hash)
	if !subtitleHashPattern.MatchString(hash) || !ok {
		http.NotFound(w, req)
		return
	}
	// Uploaded content is untrusted: never let a browser treat it as a page.
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Content-Encoding", "gzip")
	if _, err := w.Write(gz); err != nil {
		return // client went away mid-download
	}
}

// openSubtitles sends one request to the OpenSubtitles API and decodes the
// JSON reply into out.
func (a *subtitleAPI) openSubtitles(method, path string, body io.Reader, out any) error {
	if a.key == "" {
		return errors.New("OPENSUBTITLES_KEY is not set")
	}
	req, err := http.NewRequest(method, openSubtitlesAPI+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "watchparty v1")
	req.Header.Set("Api-Key", a.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opensubtitles %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponse)).Decode(out)
}

// search answers GET /searchSubtitles?title=. Failures yield an empty list,
// which the client shows as "no results".
func (a *subtitleAPI) search(w http.ResponseWriter, req *http.Request) {
	result := struct {
		Data json.RawMessage `json:"data"`
	}{}
	title := req.URL.Query().Get("title")
	if len(title) > 200 {
		title = title[:200]
	}
	if title != "" {
		params := url.Values{"languages": {"en"}, "query": {title}}
		if err := a.openSubtitles(http.MethodGet, "subtitles?"+params.Encode(), nil, &result); err != nil {
			log.Printf("subtitle search: %v", err)
		}
	}
	if len(result.Data) == 0 {
		result.Data = json.RawMessage("[]")
	}
	writeJSON(w, result.Data)
}

// fetchSubtitleFile downloads the file OpenSubtitles points at. The link host
// comes from their API, not from the user.
func (a *subtitleAPI) fetchSubtitleFile(link string) ([]byte, error) {
	if !strings.HasPrefix(link, "https://") {
		return nil, errors.New("unexpected subtitle link")
	}
	resp, err := a.http.Get(link)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subtitle file: status %d", resp.StatusCode)
	}
	text, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(text) > maxSubtitleBytes {
		return nil, errors.New("subtitle file too large")
	}
	return text, nil
}

// fetch answers GET /downloadSubtitles?file_id= by copying the file into the
// local store and returning a server-relative link to it.
func (a *subtitleAPI) fetch(w http.ResponseWriter, req *http.Request) {
	fileID := req.URL.Query().Get("file_id")
	body, err := json.Marshal(map[string]string{"file_id": fileID})
	if err != nil || fileID == "" || len(fileID) > 32 {
		http.Error(w, "invalid file_id", http.StatusBadRequest)
		return
	}
	var ticket struct {
		Link string `json:"link"`
	}
	err = a.openSubtitles(http.MethodPost, "download", bytes.NewReader(body), &ticket)
	var text []byte
	if err == nil {
		text, err = a.fetchSubtitleFile(ticket.Link)
	}
	var hash string
	if err == nil {
		hash, err = a.store.put(text, time.Now())
	}
	if err != nil {
		log.Printf("subtitle download: %v", err)
		http.Error(w, "subtitle download failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"link": "/subtitle/" + hash})
}
