package companion

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/dlna"
)

// What casting apps are told: one MediaContainer of three Timelines, always all three, because a
// controller draws its buttons from them and reads the volume from the music one before anything
// plays. A controller polls (wait=1 holds the request until something changes), or subscribes and is
// pushed to.
//
// No token is ever in a timeline: any device on the network can ask for one, so a token there would be
// handed to anybody who asked.

const (
	// pollHold is the longest a wait=1 poll is held when nothing changes. Plex Web waits 20s for one,
	// and treats every answer as news: answered each second while nothing played, the "stopped" in
	// each one closed its Resume dialog before it could be clicked.
	pollHold = 10 * time.Second
	subFor   = 90 * time.Second
	// mostHeld caps the polls held open at once.
	mostHeld = 16
)

type timeline struct {
	p *Player

	mu      sync.Mutex
	subs    map[string]*subscription
	changed chan struct{} // closed and replaced on every push
	held    chan struct{}
}

type subscription struct {
	url     string
	command string
	until   time.Time
}

func newTimeline(p *Player) *timeline {
	return &timeline{p: p, subs: map[string]*subscription{}, changed: make(chan struct{}), held: make(chan struct{}, mostHeld)}
}

// snapshot is one moment of the player, taken under its lock.
type snapshot struct {
	state           string
	pos, dur        time.Duration
	req             mediaRequest
	it              *item
	video           bool
	volume          int
	shuffle, repeat int
	audio, subtitle int
	queue, item     string
	version         string
}

func (p *Player) snapshot() snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	sn := snapshot{volume: p.vol}
	if sn.volume < 0 {
		sn.volume = 50
	}
	s := p.cur
	if s == nil {
		return sn
	}
	sn.state, sn.pos, sn.req, sn.it = s.state, s.position(time.Now()), s.req, s.it
	sn.dur, sn.video = s.it.dur, s.it.video
	sn.shuffle, sn.repeat, sn.queue, sn.item = s.shuffle, s.repeat, s.queueID, s.itemID
	sn.version = s.queueVersion
	sn.audio = s.it.part.selected(streamAudio)
	if s.subtitles {
		sn.subtitle = s.it.part.selected(streamSubtitle)
	}
	return sn
}

// body is the timeline XML. Its location is where the player is (a film or music on the screen, or
// nothing): Plex Web moves its progress bar only for a timeline that says, whether it polled or was
// pushed to.
func (t *timeline) body(command string) string {
	s := t.p.snapshot()
	var b strings.Builder
	// The player's identifier in the body too: through the server, the header that carries it is not
	// one a browser may read (the server sets its own Access-Control-Expose-Headers).
	b.WriteString(`<MediaContainer location="` + s.location() + `" machineIdentifier="` + dlna.Escape(t.p.ID.ClientID()) + `"`)
	if command != "" {
		b.WriteString(` commandID="` + dlna.Escape(command) + `"`)
	}
	b.WriteString(`>`)
	for _, typ := range []string{"music", "video", "photo"} {
		b.WriteString(s.timeline(typ))
	}
	b.WriteString(`</MediaContainer>`)
	return b.String()
}

func (s snapshot) location() string {
	if s.state == "" || s.state == stateStopped || s.state == stateError {
		return "navigation"
	}
	switch s.kind() {
	case "video":
		return "fullScreenVideo"
	case "music":
		return "fullScreenMusic"
	}
	return "navigation"
}

func (s snapshot) kind() string {
	if s.it == nil {
		return ""
	}
	if s.video {
		return "video"
	}
	return "music"
}

// timeline is one media type's entry. The fields are the item's coordinates and where it is; the
// server's token is not among them.
func (s snapshot) timeline(typ string) string {
	if s.kind() != typ {
		if typ == "photo" {
			return `<Timeline type="photo" state="stopped"/>`
		}
		return `<Timeline type="` + typ + `" state="stopped" volume="` + strconv.Itoa(s.volume) +
			`" controllable="playPause,stop,volume"/>`
	}
	var b strings.Builder
	attr := func(name, value string) {
		if value != "" {
			b.WriteString(` ` + name + `="` + dlna.Escape(value) + `"`)
		}
	}
	b.WriteString(`<Timeline type="` + typ + `"`)
	attr("state", s.state)
	attr("time", strconv.FormatInt(s.pos.Milliseconds(), 10))
	attr("duration", strconv.FormatInt(s.dur.Milliseconds(), 10))
	attr("ratingKey", s.it.ratingKey)
	attr("key", s.it.key)
	attr("containerKey", s.it.queue)
	attr("machineIdentifier", s.req.machine)
	// Plex Web finds the item's media, and so its audio and subtitle choices, by provider, media and
	// part: without them its track menu fails with a playback error and stops the film. Only items
	// from a server's library are played, one media and part each.
	attr("providerIdentifier", "com.plexapp.plugins.library")
	attr("mediaIndex", "0")
	attr("partIndex", "0")
	attr("address", s.req.address)
	attr("port", s.req.port)
	attr("protocol", s.req.protocol)
	attr("volume", strconv.Itoa(s.volume))
	attr("shuffle", strconv.Itoa(s.shuffle))
	attr("repeat", strconv.Itoa(s.repeat))
	attr("playQueueID", s.queue)
	attr("playQueueItemID", s.item)
	attr("playQueueVersion", s.version)
	if s.audio != 0 {
		attr("audioStreamID", strconv.Itoa(s.audio))
	}
	if s.video {
		attr("subtitleStreamID", strconv.Itoa(s.subtitle))
	}
	attr("seekRange", "0-"+strconv.FormatInt(s.dur.Milliseconds(), 10))
	controls := "playPause,stop,volume,seekTo,stepBack,stepForward,skipPrevious,skipNext,shuffle,repeat"
	if s.video {
		controls += ",audioStream,subtitleStream"
	}
	attr("controllable", controls)
	b.WriteString(`/>`)
	return b.String()
}

// wake lets every held poll go.
func (t *timeline) wake() {
	t.mu.Lock()
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
}

// push lets held polls go and sends the timeline to every live subscriber.
func (t *timeline) push() {
	t.wake()
	now := time.Now()
	t.mu.Lock()
	type out struct{ url, command string }
	var send []out
	for id, s := range t.subs {
		if now.After(s.until) {
			delete(t.subs, id)
			continue
		}
		send = append(send, out{s.url, s.command})
	}
	t.mu.Unlock()
	for _, s := range send {
		go t.post(s.url, t.body(s.command))
	}
}

func (t *timeline) post(url, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("X-Plex-Client-Identifier", t.p.ID.ClientID())
	req.Header.Set("X-Plex-Device-Name", t.p.ID.Name())
	resp, err := t.p.Push.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
}

// command records the last commandID a subscribed controller sent.
func (t *timeline) command(client, command string) {
	if client == "" {
		return
	}
	t.mu.Lock()
	if s := t.subs[client]; s != nil {
		s.command = command
	}
	t.mu.Unlock()
}

// add records a subscriber, unless the cap is reached by others.
func (t *timeline) add(id, url, command string, most int) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, s := range t.subs {
		if now.After(s.until) {
			delete(t.subs, k)
		}
	}
	if _, ok := t.subs[id]; !ok && len(t.subs) >= most {
		return false
	}
	t.subs[id] = &subscription{url: url, command: command, until: now.Add(subFor)}
	return true
}

func (t *timeline) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.subs)
}

// poll answers a controller that asks rather than subscribes.
func (p *Player) poll(w http.ResponseWriter, r *http.Request) {
	if !readable(w, r) {
		return
	}
	q := query{r}
	if q.get("wait") == "1" {
		select {
		case p.tl.held <- struct{}{}:
			p.tl.mu.Lock()
			ch := p.tl.changed
			p.tl.mu.Unlock()
			select {
			case <-ch:
			case <-time.After(pollHold):
			case <-r.Context().Done():
			}
			<-p.tl.held
			if r.Context().Err() != nil {
				return
			}
		default: // too many held already: answered at once
		}
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, p.tl.body(q.get("commandID")))
}

// subscribe registers a controller to be pushed to: at the address the request came from, on the port
// it names.
func (p *Player) subscribe(w http.ResponseWriter, r *http.Request) {
	if !readable(w, r) {
		return
	}
	q := query{r}
	id := r.Header.Get("X-Plex-Client-Identifier")
	if id == "" || id == p.ID.ClientID() {
		fail(w, http.StatusBadRequest, "no controller")
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	protocol := "http"
	if q.get("protocol") == "https" {
		protocol = "https"
	}
	port := q.number("port", 32400)
	if port < 1 || port > 65535 {
		fail(w, http.StatusBadRequest, "no port")
		return
	}
	u := protocol + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/:/timeline"
	if !p.tl.add(id, u, q.get("commandID"), p.MaxSubscribers) {
		fail(w, http.StatusServiceUnavailable, "too many controllers")
		return
	}
	go p.tl.post(u, p.tl.body(q.get("commandID")))
	agree(w)
}

func (p *Player) unsubscribe(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Plex-Client-Identifier")
	p.tl.mu.Lock()
	delete(p.tl.subs, id)
	p.tl.mu.Unlock()
	agree(w)
}

// Busy is whether something is the player's now, for the status page.
func (p *Player) Busy() (title, state string) {
	s := p.snapshot()
	if s.it == nil {
		return "", ""
	}
	return s.it.title, s.state
}
