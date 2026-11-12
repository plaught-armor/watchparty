package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	alice = "11111111-1111-4111-8111-111111111111"
	bob   = "22222222-2222-4222-8222-222222222222"
)

type frame struct {
	event string
	data  json.RawMessage
}

// drain returns every frame queued for c and empties the queue.
func drain(t *testing.T, c *Conn) []frame {
	t.Helper()
	var frames []frame
	for range sendQueueSize {
		select {
		case raw := <-c.send:
			var parts []json.RawMessage
			if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != 2 {
				t.Fatalf("bad frame %s: %v", raw, err)
			}
			var f frame
			if err := json.Unmarshal(parts[0], &f.event); err != nil {
				t.Fatalf("bad event in %s: %v", raw, err)
			}
			f.data = parts[1]
			frames = append(frames, f)
			continue
		default:
		}
		break
	}
	return frames
}

func events(frames []frame) string {
	names := make([]string, len(frames))
	for i, f := range frames {
		names[i] = f.event
	}
	return strings.Join(names, ",")
}

func find(frames []frame, event string) *frame {
	for i := range frames {
		if frames[i].event == event {
			return &frames[i]
		}
	}
	return nil
}

// joined returns a room with alice and bob connected and their queues empty.
func joined(t *testing.T) (*Room, *Conn, *Conn) {
	t.Helper()
	r := newRoom("test")
	a, b := newConn(nil, alice), newConn(nil, bob)
	r.join(a)
	r.join(b)
	drain(t, a)
	drain(t, b)
	return r, a, b
}

func send(r *Room, c *Conn, event string, data any) {
	raw, err := json.Marshal([]any{event, data})
	if err != nil {
		panic(err)
	}
	r.dispatch(c, newYoutubeClient(""), raw)
}

func TestJoinSendsFullState(t *testing.T) {
	r := newRoom("test")
	a := newConn(nil, alice)
	r.join(a)
	got := events(drain(t, a))
	want := "connect,REC:host,REC:nameMap,REC:pictureMap,REC:tsMap,REC:lock,chatinit,playlist,roster"
	if got != want {
		t.Fatalf("join frames = %s, want %s", got, want)
	}
}

func TestRejoinReplacesOldConnection(t *testing.T) {
	r, a, _ := joined(t)
	a2 := newConn(nil, alice)
	r.join(a2)
	select {
	case <-a.stopped:
	default:
		t.Fatal("old connection was not stopped")
	}
	if a.stopCode != closeKicked {
		t.Fatalf("old connection close code = %d, want %d", a.stopCode, closeKicked)
	}
	// The old connection's late cleanup must not evict its replacement.
	r.leave(a)
	if r.conns[alice] != a2 || len(r.roster) != 2 {
		t.Fatalf("replacement lost: conns=%d roster=%d", len(r.conns), len(r.roster))
	}
}

func TestLeaveRemovesFromRoster(t *testing.T) {
	r, a, b := joined(t)
	send(r, a, "CMD:name", "Alice")
	drain(t, b)
	r.leave(a)
	roster := find(drain(t, b), "roster")
	if roster == nil || strings.Contains(string(roster.data), alice) {
		t.Fatalf("roster after leave = %v", roster)
	}
	if _, ok := r.nameMap[alice]; ok {
		t.Fatal("name of a silent client was kept after leaving")
	}
}

func TestSeekGoesToOthersOnly(t *testing.T) {
	r, a, b := joined(t)
	send(r, a, "CMD:seek", 42.5)
	if got := events(drain(t, a)); got != "REC:chat" {
		t.Fatalf("sender frames = %s, want REC:chat", got)
	}
	seek := find(drain(t, b), "REC:seek")
	if seek == nil || string(seek.data) != "42.5" {
		t.Fatalf("receiver seek = %v", seek)
	}
	if r.videoTS != 42.5 {
		t.Fatalf("videoTS = %v", r.videoTS)
	}
}

func TestMalformedInputIsIgnored(t *testing.T) {
	r, a, b := joined(t)
	for _, raw := range []string{
		``, `{}`, `[]`, `[1]`, `["CMD:seek","x"]`, `["CMD:seek",null]`, `["CMD:seek"]`,
		`["CMD:chat",""]`, `["CMD:chat",5]`, `["CMD:name",""]`, `["CMD:playbackRate",100]`,
		`["CMD:playlistDelete",0]`, `["CMD:playlistDelete",-1]`, `["CMD:playlistDelete",0.5]`,
		`["CMD:playlistMove",{"index":0,"toIndex":1}]`, `["CMD:addReaction",{"value":"x"}]`,
		`["nope",1]`, `["CMD:host",5]`, `["CMD:kickUser",{"userToBeKicked":"ghost"}]`,
	} {
		r.dispatch(a, newYoutubeClient(""), []byte(raw))
	}
	if got := events(drain(t, b)); got != "" {
		t.Fatalf("malformed input produced frames: %s", got)
	}
}

func TestHostResetsPlayback(t *testing.T) {
	r, a, b := joined(t)
	r.playbackRate, r.loop, r.subtitle, r.videoTS = 2, true, "sub", 99
	send(r, a, "CMD:host", "https://example.com/a.mp4")
	if r.video != "https://example.com/a.mp4" || r.videoTS != 0 || r.playbackRate != 1 || r.loop || r.subtitle != "" {
		t.Fatalf("state not reset: %+v", r.hostState())
	}
	if got := events(drain(t, b)); got != "REC:tsMap,REC:host,REC:chat,roster" {
		t.Fatalf("frames = %s", got)
	}
	// Timestamps sent right after the switch belong to the previous video.
	send(r, a, "CMD:ts", 500)
	if r.videoTS != 0 {
		t.Fatalf("stale timestamp accepted: %v", r.videoTS)
	}
	r.preventTSUntil = time.Time{}
	send(r, a, "CMD:ts", 12)
	if r.videoTS != 12 {
		t.Fatalf("videoTS = %v, want 12", r.videoTS)
	}
}

func TestChatHistoryIsCapped(t *testing.T) {
	r, a, _ := joined(t)
	for i := range maxChatMessages + 20 {
		send(r, a, "CMD:chat", fmt.Sprintf("m%d", i))
		drain(t, a)
	}
	if len(r.chat) != maxChatMessages || r.chat[0].Msg != "m20" {
		t.Fatalf("chat len=%d first=%q", len(r.chat), r.chat[0].Msg)
	}
}

func TestReplyAndReactions(t *testing.T) {
	r, a, b := joined(t)
	send(r, a, "CMD:chat", "hello")
	first := r.chat[0]
	send(r, b, "CMD:chatV2", map[string]string{
		"msg": "hi back", "replyToId": first.ID, "replyToTimestamp": first.Timestamp,
	})
	reply := r.chat[1]
	if reply.ReplyToMsg != "hello" || reply.ReplyToUserID != alice {
		t.Fatalf("reply = %+v", reply)
	}
	// A reply with only half of the target is rejected outright.
	send(r, b, "CMD:chatV2", map[string]string{"msg": "x", "replyToId": first.ID})
	if len(r.chat) != 2 {
		t.Fatalf("half reply accepted, chat len %d", len(r.chat))
	}
	reaction := map[string]string{"value": "👍", "msgId": first.ID, "msgTimestamp": first.Timestamp}
	send(r, b, "CMD:addReaction", reaction)
	send(r, b, "CMD:addReaction", reaction)
	if got := r.chat[0].Reactions["👍"]; len(got) != 1 || got[0] != bob {
		t.Fatalf("reactions = %v", got)
	}
	send(r, b, "CMD:removeReaction", reaction)
	if got := r.chat[0].Reactions["👍"]; len(got) != 0 {
		t.Fatalf("reaction not removed: %v", got)
	}
	reaction["value"] = strings.Repeat("x", maxReactionUnits+1)
	send(r, b, "CMD:addReaction", reaction)
	if len(r.chat[0].Reactions) != 1 {
		t.Fatalf("oversized reaction accepted: %v", r.chat[0].Reactions)
	}
}

func TestDeleteChatMessages(t *testing.T) {
	r, a, b := joined(t)
	send(r, a, "CMD:chat", "a1")
	send(r, b, "CMD:chat", "b1")
	send(r, a, "CMD:chat", "a2")
	send(r, a, "CMD:deleteChatMessages", map[string]string{"author": alice})
	if len(r.chat) != 1 || r.chat[0].Msg != "b1" {
		t.Fatalf("chat after author delete = %+v", r.chat)
	}
	send(r, a, "CMD:deleteChatMessages", map[string]string{})
	if len(r.chat) != 0 {
		t.Fatalf("chat after clear = %+v", r.chat)
	}
}

func TestPlaylist(t *testing.T) {
	r, a, _ := joined(t)
	for _, link := range []string{"u1", "u2", "u3", "u4"} {
		send(r, a, "CMD:playlistAdd", link)
	}
	// The first add starts playing because the room was idle.
	if r.video != "u1" || len(r.playlist) != 3 {
		t.Fatalf("video=%q playlist=%d", r.video, len(r.playlist))
	}
	send(r, a, "CMD:playlistMove", map[string]int{"index": 0, "toIndex": 2})
	if got := r.playlist[0].URL + r.playlist[1].URL + r.playlist[2].URL; got != "u3u4u2" {
		t.Fatalf("after move = %s", got)
	}
	send(r, a, "CMD:playlistDelete", 1)
	if got := r.playlist[0].URL + r.playlist[1].URL; got != "u3u2" {
		t.Fatalf("after delete = %s", got)
	}
	r.preventTSUntil = time.Time{}
	// Two viewers report the same video ending; only the first advances.
	send(r, a, "CMD:playlistNext", "u1")
	r.preventTSUntil = time.Time{}
	send(r, a, "CMD:playlistNext", "u1")
	if r.video != "u3" || len(r.playlist) != 1 {
		t.Fatalf("after next: video=%q playlist=%d", r.video, len(r.playlist))
	}
	// Clearing the video pulls the next one from the playlist.
	send(r, a, "CMD:host", "")
	if r.video != "u2" || len(r.playlist) != 0 {
		t.Fatalf("after clear: video=%q playlist=%d", r.video, len(r.playlist))
	}
}

func TestScreenShare(t *testing.T) {
	r, a, b := joined(t)
	send(r, a, "CMD:joinScreenShare", map[string]bool{"file": false})
	if r.video != "screenshare://"+alice {
		t.Fatalf("video = %q", r.video)
	}
	roster := find(drain(t, b), "roster")
	if roster == nil || !strings.Contains(string(roster.data), `"isScreenShare":true`) {
		t.Fatalf("roster = %v", roster)
	}
	send(r, b, "CMD:joinScreenShare", map[string]bool{"file": true})
	send(r, b, "CMD:leaveScreenShare", nil)
	if got := events(drain(t, b)); got != "errorMessage,errorMessage" {
		t.Fatalf("second sharer frames = %s", got)
	}
	send(r, a, "CMD:leaveScreenShare", nil)
	if r.video != "" {
		t.Fatalf("video after leave = %q", r.video)
	}
}

func TestKickAndSignal(t *testing.T) {
	r, a, b := joined(t)
	send(r, a, "signal", map[string]any{"to": bob, "msg": map[string]int{"sdp": 1}})
	signal := find(drain(t, b), "signal")
	if signal == nil || !strings.Contains(string(signal.data), `"from":"`+alice) {
		t.Fatalf("signal = %v", signal)
	}
	send(r, a, "CMD:kickUser", map[string]string{"userToBeKicked": bob})
	if got := events(drain(t, b)); got != "kicked" || b.stopCode != closeKicked {
		t.Fatalf("kick frames=%s code=%d", got, b.stopCode)
	}
}

func TestSlowConsumerIsDropped(t *testing.T) {
	r, a, b := joined(t)
	for range sendQueueSize + 5 {
		send(r, a, "CMD:loop", true)
		drain(t, a)
	}
	select {
	case <-b.stopped:
	default:
		t.Fatal("stalled connection was not stopped")
	}
}

func TestReapable(t *testing.T) {
	r := newRoom("test")
	now := time.Now()
	if r.reapable(now) || !r.reapable(now.Add(2*time.Minute)) {
		t.Fatal("blank room should be reaped after a minute")
	}
	r.video = "u1"
	if r.reapable(now.Add(time.Hour)) || !r.reapable(now.Add(9*time.Hour)) {
		t.Fatal("used room should be kept for 8 hours")
	}
	r.join(newConn(nil, alice))
	if r.reapable(now.Add(9 * time.Hour)) {
		t.Fatal("occupied room must never be reaped")
	}
}

func TestClaimSession(t *testing.T) {
	h := newHub(Config{MaxRooms: 1, MaxConns: 1}, newYoutubeClient(""), nil)
	now := time.Now()
	const s1, s2 = "session-aaaaaaaaaaaa", "session-bbbbbbbbbbbb"
	if h.claimSession(alice, "short", now) {
		t.Fatal("short session ID accepted")
	}
	if !h.claimSession(alice, s1, now) || !h.claimSession(alice, s1, now) {
		t.Fatal("owner rejected")
	}
	if h.claimSession(alice, s2, now) {
		t.Fatal("another session took over a client ID")
	}
	if !h.claimSession(alice, s2, now.Add(sessionTTL+time.Hour)) {
		t.Fatal("expired claim still enforced")
	}
}

func TestHubLimits(t *testing.T) {
	h := newHub(Config{MaxRooms: 1, MaxConns: 1, RoomCapacity: 1}, newYoutubeClient(""), nil)
	if _, err := h.joinRoom("one", newConn(nil, alice)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.joinRoom("one", newConn(nil, bob)); err != errRoomFull {
		t.Fatalf("second viewer err = %v, want room full", err)
	}
	if _, err := h.joinRoom("one", newConn(nil, alice)); err != nil {
		t.Fatalf("reconnect rejected: %v", err)
	}
	if _, err := h.joinRoom("two", newConn(nil, bob)); err != errServerFull {
		t.Fatalf("second room err = %v, want server full", err)
	}
	if !h.acquireConn() || h.acquireConn() {
		t.Fatal("connection cap not enforced")
	}
}

func TestYoutubeParsing(t *testing.T) {
	for link, want := range map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":        "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ?t=5":                   "dQw4w9WgXcQ",
		"https://www.youtube.com/embed/dQw4w9WgXcQ":          "dQw4w9WgXcQ",
		"https://www.youtube.com/watch?list=x&v=dQw4w9WgXcQ": "dQw4w9WgXcQ",
		"https://example.com/video.mp4":                      "",
	} {
		if got := youtubeVideoID(link); got != want {
			t.Errorf("youtubeVideoID(%q) = %q, want %q", link, got, want)
		}
	}
	if got := parseDuration("PT1H2M3S"); got != 3723 {
		t.Errorf("parseDuration = %d, want 3723", got)
	}
	if !sameYoutubeVideo("https://youtu.be/dQw4w9WgXcQ", "https://www.youtube.com/watch?v=dQw4w9WgXcQ") {
		t.Error("same video in two URL forms not matched")
	}
}

func TestSubtitleStore(t *testing.T) {
	s := newSubtitleStore()
	now := time.Now()
	hash, err := s.put([]byte("1\n00:00:01,000 --> 00:00:02,000\nhi\n"), now)
	if err != nil || !subtitleHashPattern.MatchString(hash) {
		t.Fatalf("put: hash=%q err=%v", hash, err)
	}
	if _, ok := s.get(hash); !ok {
		t.Fatal("stored subtitle not found")
	}
	s.prune(now.Add(subtitleTTL + time.Minute))
	if _, ok := s.get(hash); ok || s.bytes != 0 {
		t.Fatalf("expired subtitle kept, bytes=%d", s.bytes)
	}
}
