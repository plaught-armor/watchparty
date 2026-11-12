package main

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxChatMessages = 100
	maxPlaylist     = 500
	maxVideoLen     = 50000
	maxChatLen      = 10000
	maxNameLen      = 50
	maxPictureLen   = 10000
	maxSubtitleLen  = 10000
	maxPlaylistURL  = 20000
	// Emoji can span several UTF-16 units, same limit the web client assumes.
	maxReactionUnits = 8
	// Timestamps reported right after a video change belong to the old video.
	tsSettleTime = time.Second
)

type User struct {
	ID            string `json:"id"`
	IsVideoChat   bool   `json:"isVideoChat,omitempty"`
	IsMuted       bool   `json:"isMuted,omitempty"`
	IsScreenShare bool   `json:"isScreenShare"`
}

type ChatMessage struct {
	ID               string              `json:"id"`
	Cmd              string              `json:"cmd,omitempty"`
	Msg              string              `json:"msg,omitempty"`
	Timestamp        string              `json:"timestamp"`
	VideoTS          *float64            `json:"videoTS,omitempty"`
	Reactions        map[string][]string `json:"reactions,omitempty"`
	ReplyToID        string              `json:"replyToId,omitempty"`
	ReplyToTimestamp string              `json:"replyToTimestamp,omitempty"`
	ReplyToUserID    string              `json:"replyToUserId,omitempty"`
	ReplyToMsg       string              `json:"replyToMsg,omitempty"`
}

type PlaylistVideo struct {
	URL      string `json:"url"`
	Name     string `json:"name"`
	Img      string `json:"img,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Duration int    `json:"duration"`
	Type     string `json:"type"`
}

type HostState struct {
	Video           string  `json:"video"`
	VideoTS         float64 `json:"videoTS"`
	Subtitle        string  `json:"subtitle"`
	PlaybackRate    float64 `json:"playbackRate"`
	Paused          bool    `json:"paused"`
	IsVBrowserLarge bool    `json:"isVBrowserLarge"`
	Loop            bool    `json:"loop"`
}

type Reaction struct {
	User         string `json:"user"`
	Value        string `json:"value"`
	MsgID        string `json:"msgId"`
	MsgTimestamp string `json:"msgTimestamp"`
}

// Room holds one watch party. Every field below mu is guarded by it.
type Room struct {
	id string

	mu           sync.Mutex
	video        string
	videoTS      float64
	subtitle     string
	playbackRate float64
	paused       bool
	loop         bool
	chat         []ChatMessage
	nameMap      map[string]string
	pictureMap   map[string]string
	playlist     []PlaylistVideo
	// roster keeps join order; conns maps a clientID to its current connection.
	roster         []User
	conns          map[string]*Conn
	tsMap          map[string]float64
	lastTsMap      time.Time
	preventTSUntil time.Time
	lastActive     time.Time
}

func newRoom(id string) *Room {
	now := time.Now()
	return &Room{
		id:           id,
		playbackRate: 1,
		chat:         []ChatMessage{},
		nameMap:      map[string]string{},
		pictureMap:   map[string]string{},
		playlist:     []PlaylistVideo{},
		conns:        map[string]*Conn{},
		tsMap:        map[string]float64{},
		lastTsMap:    now,
		lastActive:   now,
	}
}

// encode builds one wire frame: a JSON array of [event, data].
func encode(event string, data any) []byte {
	frame, err := json.Marshal([2]any{event, data})
	if err != nil {
		log.Printf("encode %s: %v", event, err)
		return nil
	}
	return frame
}

func (r *Room) emit(event string, data any) {
	r.emitExcept(nil, event, data)
}

func (r *Room) emitExcept(skip *Conn, event string, data any) {
	frame := encode(event, data)
	if frame == nil {
		return
	}
	for _, c := range r.conns {
		if c != skip {
			c.push(frame)
		}
	}
}

func (r *Room) hostState() HostState {
	return HostState{
		Video:        r.video,
		VideoTS:      r.videoTS,
		Subtitle:     r.subtitle,
		PlaybackRate: r.playbackRate,
		Paused:       r.paused,
		Loop:         r.loop,
	}
}

// sharerID returns the clientID embedded in a screenshare/fileshare video URL.
func (r *Room) sharerID() string {
	for _, prefix := range []string{"screenshare://", "fileshare://"} {
		if rest, ok := strings.CutPrefix(r.video, prefix); ok {
			id, _, _ := strings.Cut(rest, "@")
			return id
		}
	}
	return ""
}

func (r *Room) rosterForApp() []User {
	sharer := r.sharerID()
	out := make([]User, len(r.roster))
	for i, u := range r.roster {
		u.IsScreenShare = u.ID == sharer
		out[i] = u
	}
	return out
}

func (r *Room) rosterIndex(clientID string) int {
	for i := range r.roster {
		if r.roster[i].ID == clientID {
			return i
		}
	}
	return -1
}

// join registers c, replacing any older connection of the same client, and
// sends it the full room state. Caller holds r.mu.
func (r *Room) join(c *Conn) {
	if old := r.conns[c.clientID]; old != nil {
		old.kick()
	}
	r.conns[c.clientID] = c
	if r.rosterIndex(c.clientID) < 0 {
		r.roster = append(r.roster, User{ID: c.clientID})
	}
	c.emit("connect", nil)
	c.emit("REC:host", r.hostState())
	c.emit("REC:nameMap", r.nameMap)
	c.emit("REC:pictureMap", r.pictureMap)
	c.emit("REC:tsMap", r.tsMap)
	c.emit("REC:lock", "")
	c.emit("chatinit", r.chat)
	c.emit("playlist", r.playlist)
	r.emit("roster", r.rosterForApp())
}

func (r *Room) leave(c *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A replaced connection must not remove the client that replaced it.
	if r.conns[c.clientID] != c {
		return
	}
	delete(r.conns, c.clientID)
	delete(r.tsMap, c.clientID)
	if i := r.rosterIndex(c.clientID); i >= 0 {
		r.roster = append(r.roster[:i], r.roster[i+1:]...)
	}
	// Names and pictures are only needed to render chat history, so drop them
	// for clients that never spoke. Keeps the maps bounded by the chat size.
	if !r.hasChatFrom(c.clientID) {
		delete(r.nameMap, c.clientID)
		delete(r.pictureMap, c.clientID)
	}
	r.lastActive = time.Now()
	r.emit("roster", r.rosterForApp())
}

func (r *Room) hasChatFrom(clientID string) bool {
	for i := range r.chat {
		if r.chat[i].ID == clientID {
			return true
		}
	}
	return false
}

// tick broadcasts viewer timestamps; the hub calls it once per second.
func (r *Room) tick(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.video == "" || len(r.conns) == 0 {
		return
	}
	r.lastTsMap = now
	r.emit("REC:tsMap", r.tsMap)
}

// reapable reports whether an empty room has been idle long enough to drop.
// Rooms that never held anything go quickly so stray connects can't fill the hub.
func (r *Room) reapable(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.conns) > 0 {
		return false
	}
	blank := r.video == "" && len(r.chat) == 0 && len(r.playlist) == 0
	if blank {
		return now.Sub(r.lastActive) > time.Minute
	}
	return now.Sub(r.lastActive) > 8*time.Hour
}

func (r *Room) addChat(c *Conn, msg ChatMessage) {
	msg.Timestamp = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if c != nil {
		if ts, ok := r.tsMap[c.clientID]; ok {
			msg.VideoTS = &ts
		}
	}
	r.chat = append(r.chat, msg)
	if extra := len(r.chat) - maxChatMessages; extra > 0 {
		// Copy down instead of reslicing so the backing array can't grow forever.
		r.chat = append(r.chat[:0], r.chat[extra:]...)
	}
	r.emit("REC:chat", msg)
}

func (r *Room) findChat(id, timestamp string) *ChatMessage {
	for i := range r.chat {
		if r.chat[i].ID == id && r.chat[i].Timestamp == timestamp {
			return &r.chat[i]
		}
	}
	return nil
}

// setVideo switches the room to a new video ("" clears it) and resets
// playback state. c is nil when the server itself advances the playlist.
func (r *Room) setVideo(c *Conn, video string) {
	if len(video) > maxVideoLen {
		return
	}
	r.video = video
	r.videoTS = 0
	r.paused = false
	r.subtitle = ""
	r.loop = false
	r.playbackRate = 1
	r.tsMap = map[string]float64{}
	r.preventTSUntil = time.Now().Add(tsSettleTime)
	r.emit("REC:tsMap", r.tsMap)
	r.emit("REC:host", r.hostState())
	if c != nil && video != "" {
		r.addChat(c, ChatMessage{ID: c.clientID, Cmd: "host", Msg: video})
	}
	if video == "" {
		r.playlistNext("")
	}
	// The sharer flag in the roster derives from the video URL.
	r.emit("roster", r.rosterForApp())
}

// playlistNext starts the next queued video. expected, when set, is the URL
// the caller believes is playing; a mismatch means another client already
// advanced the playlist and this call is a duplicate.
func (r *Room) playlistNext(expected string) {
	if expected != "" {
		if time.Now().Before(r.preventTSUntil) {
			return
		}
		if r.video != "" && expected != r.video && !sameYoutubeVideo(expected, r.video) {
			return
		}
	}
	if len(r.playlist) == 0 {
		return
	}
	next := r.playlist[0]
	r.playlist = append(r.playlist[:0], r.playlist[1:]...)
	r.emit("playlist", r.playlist)
	r.setVideo(nil, next.URL)
}

func sameYoutubeVideo(a, b string) bool {
	idA := youtubeVideoID(a)
	return idA != "" && idA == youtubeVideoID(b)
}

// playlistAdd appends a URL to the playlist, resolving YouTube metadata first.
// It does network I/O, so the caller must NOT hold r.mu.
func (r *Room) playlistAdd(c *Conn, yt *youtubeClient, url string) {
	if url == "" || len(url) > maxPlaylistURL {
		return
	}
	item := PlaylistVideo{Name: url, Channel: "Video URL", URL: url, Type: "file"}
	if strings.HasPrefix(url, "magnet:") {
		item.Type = "magnet"
	}
	if id := youtubeVideoID(url); id != "" {
		video, err := yt.video(id)
		if err != nil {
			// The bare URL still plays, it just has no title or thumbnail.
			log.Printf("youtube lookup %s: %v", id, err)
		} else if video != nil {
			item = *video
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.playlist) >= maxPlaylist {
		if c != nil {
			c.emit("errorMessage", "The playlist is full")
		}
		return
	}
	r.playlist = append(r.playlist, item)
	r.emit("playlist", r.playlist)
	if c != nil {
		r.addChat(c, ChatMessage{ID: c.clientID, Cmd: "playlistAdd", Msg: url})
	}
	if r.video == "" {
		r.playlistNext("")
	}
}

// dispatch decodes one client frame and runs its handler.
func (r *Room) dispatch(c *Conn, yt *youtubeClient, raw []byte) {
	var frame []json.RawMessage
	if err := json.Unmarshal(raw, &frame); err != nil || len(frame) == 0 || len(frame) > 2 {
		return
	}
	var event string
	if err := json.Unmarshal(frame[0], &event); err != nil {
		return
	}
	var data json.RawMessage
	if len(frame) == 2 {
		data = frame[1]
	}
	// The only command that does network I/O, so it takes the lock itself.
	if event == "CMD:playlistAdd" {
		if url, ok := asString(data); ok {
			r.playlistAdd(c, yt, url)
		}
		return
	}
	handler, ok := handlers[event]
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	handler(r, c, data)
}

// handlers run with r.mu held. Input is untrusted: each one validates its
// payload and silently ignores anything malformed.
var handlers = map[string]func(r *Room, c *Conn, data json.RawMessage){
	"CMD:name":               cmdName,
	"CMD:picture":            cmdPicture,
	"CMD:host":               cmdHost,
	"CMD:play":               cmdPlay,
	"CMD:pause":              cmdPause,
	"CMD:seek":               cmdSeek,
	"CMD:playbackRate":       cmdPlaybackRate,
	"CMD:loop":               cmdLoop,
	"CMD:ts":                 cmdTS,
	"CMD:chat":               cmdChat,
	"CMD:chatV2":             cmdChat,
	"CMD:addReaction":        cmdAddReaction,
	"CMD:removeReaction":     cmdRemoveReaction,
	"CMD:joinVideo":          cmdJoinVideo,
	"CMD:leaveVideo":         cmdLeaveVideo,
	"CMD:userMute":           cmdUserMute,
	"CMD:joinScreenShare":    cmdJoinScreenShare,
	"CMD:leaveScreenShare":   cmdLeaveScreenShare,
	"CMD:subtitle":           cmdSubtitle,
	"CMD:lock":               cmdLock,
	"CMD:askHost":            cmdAskHost,
	"CMD:playlistNext":       cmdPlaylistNext,
	"CMD:playlistMove":       cmdPlaylistMove,
	"CMD:playlistDelete":     cmdPlaylistDelete,
	"CMD:kickUser":           cmdKickUser,
	"CMD:deleteChatMessages": cmdDeleteChatMessages,
	"CMD:startVBrowser":      cmdUnsupported("Virtual browsers are not available on this server"),
	"CMD:setRoomState":       cmdUnsupported("Room settings are not available on this server"),
	"CMD:setRoomOwner":       cmdUnsupported("Permanent rooms are not available on this server"),
	"signal":                 cmdSignal("signal"),
	"signalSS":               cmdSignal("signalSS"),
}

func asString(data json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return "", false
	}
	return s, true
}

func asNumber(data json.RawMessage) (float64, bool) {
	var n *float64
	if err := json.Unmarshal(data, &n); err != nil || n == nil {
		return 0, false
	}
	return *n, true
}

func formatNumber(n float64) string {
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func cmdUnsupported(message string) func(*Room, *Conn, json.RawMessage) {
	return func(_ *Room, c *Conn, _ json.RawMessage) {
		c.emit("errorMessage", message)
	}
}

func cmdName(r *Room, c *Conn, data json.RawMessage) {
	name, ok := asString(data)
	if !ok || name == "" || utf8.RuneCountInString(name) > maxNameLen {
		return
	}
	r.nameMap[c.clientID] = name
	r.emit("REC:nameMap", r.nameMap)
}

func cmdPicture(r *Room, c *Conn, data json.RawMessage) {
	picture, ok := asString(data)
	if !ok || len(picture) > maxPictureLen {
		return
	}
	r.pictureMap[c.clientID] = picture
	r.emit("REC:pictureMap", r.pictureMap)
}

func cmdHost(r *Room, c *Conn, data json.RawMessage) {
	if video, ok := asString(data); ok {
		r.setVideo(c, video)
	}
}

// playbackChat announces a play/pause/seek along with the sender's position.
func (r *Room) playbackChat(c *Conn, cmd string) {
	msg := ChatMessage{ID: c.clientID, Cmd: cmd}
	if ts, ok := r.tsMap[c.clientID]; ok {
		msg.Msg = formatNumber(ts)
	}
	r.addChat(c, msg)
}

func cmdPlay(r *Room, c *Conn, _ json.RawMessage) {
	r.emitExcept(c, "REC:play", r.video)
	r.paused = false
	r.playbackChat(c, "play")
}

func cmdPause(r *Room, c *Conn, _ json.RawMessage) {
	r.emitExcept(c, "REC:pause", nil)
	r.paused = true
	r.playbackChat(c, "pause")
}

func cmdSeek(r *Room, c *Conn, data json.RawMessage) {
	ts, ok := asNumber(data)
	if !ok {
		return
	}
	r.videoTS = ts
	r.emitExcept(c, "REC:seek", ts)
	r.addChat(c, ChatMessage{ID: c.clientID, Cmd: "seek", Msg: formatNumber(ts)})
}

func cmdPlaybackRate(r *Room, c *Conn, data json.RawMessage) {
	rate, ok := asNumber(data)
	// Browsers only support rates in roughly this range.
	if !ok || rate < 0.0625 || rate > 16 {
		return
	}
	r.playbackRate = rate
	r.emit("REC:playbackRate", rate)
	r.addChat(c, ChatMessage{ID: c.clientID, Cmd: "playbackRate", Msg: formatNumber(rate)})
}

func cmdLoop(r *Room, _ *Conn, data json.RawMessage) {
	var loop *bool
	if err := json.Unmarshal(data, &loop); err != nil || loop == nil {
		return
	}
	r.loop = *loop
	r.emit("REC:loop", r.loop)
}

func cmdTS(r *Room, c *Conn, data json.RawMessage) {
	ts, ok := asNumber(data)
	now := time.Now()
	if !ok || now.Before(r.preventTSUntil) {
		return
	}
	// Live streams report negative values, so those may overwrite. Otherwise
	// only move forward so a lagging viewer can't hold the room position back.
	if ts < 0 || ts > r.videoTS {
		r.videoTS = ts
	}
	// Normalize to the moment of the next tsMap broadcast, one second after the last.
	r.tsMap[c.clientID] = ts - now.Sub(r.lastTsMap).Seconds() + 1
}

// cmdChat accepts the legacy string payload and the V2 object payload.
func cmdChat(r *Room, c *Conn, data json.RawMessage) {
	var payload struct {
		Msg              string `json:"msg"`
		ReplyToID        string `json:"replyToId"`
		ReplyToTimestamp string `json:"replyToTimestamp"`
	}
	if text, ok := asString(data); ok {
		payload.Msg = text
	} else if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	if payload.Msg == "" || utf8.RuneCountInString(payload.Msg) > maxChatLen {
		return
	}
	// Reply fields come as a pair or not at all.
	if (payload.ReplyToID == "") != (payload.ReplyToTimestamp == "") {
		return
	}
	msg := ChatMessage{ID: c.clientID, Msg: payload.Msg}
	// A reply to a message that has scrolled out of history posts as a plain message.
	if target := r.findChat(payload.ReplyToID, payload.ReplyToTimestamp); target != nil {
		msg.ReplyToID = payload.ReplyToID
		msg.ReplyToTimestamp = payload.ReplyToTimestamp
		msg.ReplyToUserID = payload.ReplyToID
		msg.ReplyToMsg = target.Msg
	}
	r.addChat(c, msg)
}

// reactionTarget validates a reaction payload and finds the message it refers to.
func (r *Room) reactionTarget(c *Conn, data json.RawMessage) (*ChatMessage, Reaction) {
	var reaction Reaction
	if err := json.Unmarshal(data, &reaction); err != nil {
		return nil, reaction
	}
	if reaction.Value == "" || reaction.MsgID == "" || reaction.MsgTimestamp == "" {
		return nil, reaction
	}
	if len(utf16.Encode([]rune(reaction.Value))) > maxReactionUnits {
		return nil, reaction
	}
	reaction.User = c.clientID
	return r.findChat(reaction.MsgID, reaction.MsgTimestamp), reaction
}

func indexOf(list []string, value string) int {
	for i, v := range list {
		if v == value {
			return i
		}
	}
	return -1
}

func cmdAddReaction(r *Room, c *Conn, data json.RawMessage) {
	msg, reaction := r.reactionTarget(c, data)
	if msg == nil || indexOf(msg.Reactions[reaction.Value], c.clientID) >= 0 {
		return
	}
	if msg.Reactions == nil {
		msg.Reactions = map[string][]string{}
	}
	msg.Reactions[reaction.Value] = append(msg.Reactions[reaction.Value], c.clientID)
	r.emit("REC:addReaction", reaction)
}

func cmdRemoveReaction(r *Room, c *Conn, data json.RawMessage) {
	msg, reaction := r.reactionTarget(c, data)
	if msg == nil {
		return
	}
	users := msg.Reactions[reaction.Value]
	i := indexOf(users, c.clientID)
	if i < 0 {
		return
	}
	msg.Reactions[reaction.Value] = append(users[:i], users[i+1:]...)
	r.emit("REC:removeReaction", reaction)
}

func cmdJoinVideo(r *Room, c *Conn, _ json.RawMessage) {
	if i := r.rosterIndex(c.clientID); i >= 0 {
		r.roster[i].IsVideoChat = true
	}
	r.emit("roster", r.rosterForApp())
}

func cmdLeaveVideo(r *Room, c *Conn, _ json.RawMessage) {
	if i := r.rosterIndex(c.clientID); i >= 0 {
		r.roster[i].IsVideoChat = false
	}
	r.emit("roster", r.rosterForApp())
}

func cmdUserMute(r *Room, c *Conn, data json.RawMessage) {
	var payload struct {
		IsMuted bool `json:"isMuted"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	if i := r.rosterIndex(c.clientID); i >= 0 {
		r.roster[i].IsMuted = payload.IsMuted
	}
	r.emit("roster", r.rosterForApp())
}

func cmdJoinScreenShare(r *Room, c *Conn, data json.RawMessage) {
	var payload struct {
		File bool `json:"file"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	if sharer := r.sharerID(); sharer != "" && r.rosterIndex(sharer) >= 0 {
		c.emit("errorMessage", "There is already an active share in this room")
		return
	}
	scheme := "screenshare://"
	if payload.File {
		scheme = "fileshare://"
	}
	r.setVideo(c, scheme+c.clientID)
}

func cmdLeaveScreenShare(r *Room, c *Conn, _ json.RawMessage) {
	if r.sharerID() != c.clientID {
		c.emit("errorMessage", "Not the active sharer")
		return
	}
	r.setVideo(c, "")
}

func cmdSubtitle(r *Room, _ *Conn, data json.RawMessage) {
	subtitle, ok := asString(data)
	if !ok || len(subtitle) > maxSubtitleLen {
		return
	}
	r.subtitle = subtitle
	r.emit("REC:subtitle", r.subtitle)
}

// Locking a room needs a signed-in user, and this server has no accounts.
func cmdLock(_ *Room, c *Conn, _ json.RawMessage) {
	c.emit("errorMessage", "You must be logged in to lock the room.")
}

func cmdAskHost(r *Room, c *Conn, _ json.RawMessage) {
	c.emit("REC:host", r.hostState())
}

func cmdPlaylistNext(r *Room, _ *Conn, data json.RawMessage) {
	expected, _ := asString(data)
	r.playlistNext(expected)
}

// asIndex decodes a JSON number that must be a whole index into a list of length n.
func asIndex(n float64, length int) (int, bool) {
	if n != float64(int(n)) || n < 0 || int(n) >= length {
		return 0, false
	}
	return int(n), true
}

func cmdPlaylistMove(r *Room, _ *Conn, data json.RawMessage) {
	var payload struct {
		Index   *float64 `json:"index"`
		ToIndex *float64 `json:"toIndex"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Index == nil || payload.ToIndex == nil {
		return
	}
	from, okFrom := asIndex(*payload.Index, len(r.playlist))
	to, okTo := asIndex(*payload.ToIndex, len(r.playlist))
	if !okFrom || !okTo {
		return
	}
	item := r.playlist[from]
	r.playlist = append(r.playlist[:from], r.playlist[from+1:]...)
	r.playlist = append(r.playlist[:to], append([]PlaylistVideo{item}, r.playlist[to:]...)...)
	r.emit("playlist", r.playlist)
}

func cmdPlaylistDelete(r *Room, _ *Conn, data json.RawMessage) {
	n, ok := asNumber(data)
	if !ok {
		return
	}
	i, ok := asIndex(n, len(r.playlist))
	if !ok {
		return
	}
	r.playlist = append(r.playlist[:i], r.playlist[i+1:]...)
	r.emit("playlist", r.playlist)
}

func cmdKickUser(r *Room, _ *Conn, data json.RawMessage) {
	var payload struct {
		UserToBeKicked string `json:"userToBeKicked"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	if target := r.conns[payload.UserToBeKicked]; target != nil {
		target.emit("kicked", nil)
		target.kick()
	}
}

// cmdDeleteChatMessages removes one message (author + timestamp), everything
// by an author, or the whole history when neither is given.
func cmdDeleteChatMessages(r *Room, _ *Conn, data json.RawMessage) {
	var payload struct {
		Author    string `json:"author"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	kept := r.chat[:0]
	if payload.Author != "" || payload.Timestamp != "" {
		for _, msg := range r.chat {
			sameAuthor := msg.ID == payload.Author
			sameTime := payload.Timestamp == "" || msg.Timestamp == payload.Timestamp
			if !sameAuthor || !sameTime {
				kept = append(kept, msg)
			}
		}
	}
	r.chat = kept
	r.emit("chatinit", r.chat)
}

// cmdSignal relays WebRTC signaling between two clients in the room.
func cmdSignal(event string) func(*Room, *Conn, json.RawMessage) {
	return func(r *Room, c *Conn, data json.RawMessage) {
		var payload struct {
			To     string          `json:"to"`
			Msg    json.RawMessage `json:"msg"`
			Sharer *bool           `json:"sharer,omitempty"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return
		}
		target := r.conns[payload.To]
		if target == nil {
			return
		}
		target.emit(event, struct {
			From   string          `json:"from"`
			Msg    json.RawMessage `json:"msg"`
			Sharer *bool           `json:"sharer,omitempty"`
		}{c.clientID, payload.Msg, payload.Sharer})
	}
}
