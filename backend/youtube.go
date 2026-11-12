package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

const (
	youtubeAPI      = "https://www.googleapis.com/youtube/v3/"
	maxAPIResponse  = 2 << 20
	youtubeWatchURL = "https://www.youtube.com/watch?v="
)

var (
	youtubeIDPattern = regexp.MustCompile(`(?:youtube\.com/(?:[^/]+/.+/|(?:v|e(?:mbed)?)/|.*[?&]v=)|youtu\.be/)([^"&?/\s]{11})`)
	durationParts    = [3]struct {
		pattern *regexp.Regexp
		seconds int
	}{
		{regexp.MustCompile(`(\d{1,2})H`), 3600},
		{regexp.MustCompile(`(\d{1,2})M`), 60},
		{regexp.MustCompile(`(\d{1,2})S`), 1},
	}
)

// youtubeVideoID extracts the 11 character video ID from a YouTube URL, or "".
func youtubeVideoID(link string) string {
	if m := youtubeIDPattern.FindStringSubmatch(link); m != nil {
		return m[1]
	}
	return ""
}

// parseDuration converts an ISO 8601 duration such as PT1H2M3S to seconds.
func parseDuration(iso string) int {
	total := 0
	for _, part := range durationParts {
		if m := part.pattern.FindStringSubmatch(iso); m != nil {
			n, _ := strconv.Atoi(m[1]) // the pattern only matches digits
			total += n * part.seconds
		}
	}
	return total
}

// youtubeClient calls the YouTube Data API. With no key every lookup returns
// nothing, and the app falls back to plain URLs.
type youtubeClient struct {
	key  string
	http *http.Client
}

func newYoutubeClient(key string) *youtubeClient {
	return &youtubeClient{key: key, http: &http.Client{Timeout: 10 * time.Second}}
}

type youtubeItem struct {
	// ID is a string for videos and an object for search results.
	ID      json.RawMessage `json:"id"`
	Snippet struct {
		Title        string `json:"title"`
		ChannelTitle string `json:"channelTitle"`
		Thumbnails   struct {
			Default struct {
				URL string `json:"url"`
			} `json:"default"`
		} `json:"thumbnails"`
		ResourceID struct {
			VideoID string `json:"videoId"`
		} `json:"resourceId"`
	} `json:"snippet"`
	ContentDetails struct {
		Duration string `json:"duration"`
	} `json:"contentDetails"`
}

func (it *youtubeItem) toVideo(id string) PlaylistVideo {
	return PlaylistVideo{
		URL:      youtubeWatchURL + id,
		Name:     it.Snippet.Title,
		Img:      it.Snippet.Thumbnails.Default.URL,
		Channel:  it.Snippet.ChannelTitle,
		Duration: parseDuration(it.ContentDetails.Duration),
		Type:     "youtube",
	}
}

func (y *youtubeClient) list(resource string, params url.Values) ([]youtubeItem, error) {
	if y.key == "" {
		return nil, nil
	}
	params.Set("key", y.key)
	resp, err := y.http.Get(youtubeAPI + resource + "?" + params.Encode())
	if err != nil {
		// A *url.Error prints the full URL, which contains the API key.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("youtube %s: %w", resource, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("youtube %s: status %d", resource, resp.StatusCode)
	}
	var body struct {
		Items []youtubeItem `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponse)).Decode(&body); err != nil {
		return nil, fmt.Errorf("youtube %s: %w", resource, err)
	}
	return body.Items, nil
}

// video returns metadata for one video, or nil when it is unknown.
func (y *youtubeClient) video(id string) (*PlaylistVideo, error) {
	items, err := y.list("videos", url.Values{"part": {"snippet,contentDetails"}, "id": {id}})
	if err != nil || len(items) == 0 {
		return nil, err
	}
	v := items[0].toVideo(id)
	return &v, nil
}

func (y *youtubeClient) search(query string) ([]PlaylistVideo, error) {
	items, err := y.list("search", url.Values{
		"part": {"snippet"}, "type": {"video"}, "maxResults": {"25"}, "q": {query},
	})
	videos := make([]PlaylistVideo, 0, len(items))
	for i := range items {
		var id struct {
			VideoID string `json:"videoId"`
		}
		if json.Unmarshal(items[i].ID, &id) != nil || id.VideoID == "" {
			continue
		}
		videos = append(videos, items[i].toVideo(id.VideoID))
	}
	return videos, err
}

func (y *youtubeClient) playlist(playlistID string) ([]PlaylistVideo, error) {
	items, err := y.list("playlistItems", url.Values{
		"part": {"snippet"}, "maxResults": {"50"}, "playlistId": {playlistID},
	})
	videos := make([]PlaylistVideo, 0, len(items))
	for i := range items {
		if id := items[i].Snippet.ResourceID.VideoID; id != "" {
			videos = append(videos, items[i].toVideo(id))
		}
	}
	return videos, err
}
