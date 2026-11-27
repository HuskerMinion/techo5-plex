package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/pkg/addonapi"
	"github.com/HuskerMinion/techo5/echod/pkg/addonapi/serve"

	"github.com/HuskerMinion/techo5-plex/internal/companion"
	"github.com/HuskerMinion/techo5-plex/internal/plextv"
)

const (
	accountToken   = "account-secret-token-1"
	transientToken = "transient-secret-token-2"
	addonID        = "techo5-plex"
)

// episode is one of the fake server's episodes.
type episode struct {
	rk, show, showTitle string
	season, index       int
	watched             bool
	offset              int64 // ms
}

var episodes = []episode{
	{"200", "20", "The Office", 0, 1, false, 0},
	{"201", "20", "The Office", 1, 1, true, 0},
	{"202", "20", "The Office", 1, 2, true, 0},
	{"203", "20", "The Office", 2, 1, false, 0},
	{"204", "20", "The Office", 2, 2, false, 0},
	{"205", "20", "The Office", 2, 3, false, 0},
	{"206", "20", "The Office", 2, 4, false, 0},
	{"211", "21", "Doctor Who (2005)", 1, 1, true, 0},
	{"212", "21", "Doctor Who (2005)", 1, 2, true, 0},
	{"221", "22", "Fargo", 1, 1, true, 0},
	{"222", "22", "Fargo", 1, 2, false, 600000},
	{"223", "22", "Fargo", 2, 201, false, 0}, // a season numbered the way some libraries do: 201
}

// fakePMS is a Plex Media Server with films and shows. The library and the transient token are asked
// for with the account's token; everything to do with playing, with the transient one.
type fakePMS struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	extra    string        // another film's XML, added to the films' listing
	tok      string        // the transient token it gives, and takes ("" for transientToken)
	offset   time.Duration // where the films were left
	slow     time.Duration // how long a stream's decision takes
	slowItem time.Duration // how long an item's details take
}

func newPMS(t *testing.T) *fakePMS {
	f := &fakePMS{}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	extra, slow, transient, offset, slowItem := f.extra, f.slow, f.tok, f.offset, f.slowItem
	f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/library/metadata/") {
		time.Sleep(slowItem)
	}
	if transient == "" {
		transient = transientToken
	}
	tok := r.Header.Get("X-Plex-Token")
	if r.URL.Query().Has("X-Plex-Token") {
		http.Error(w, "the token goes as a header", 400)
		return
	}
	library := r.URL.Path == "/library/sections" || strings.HasPrefix(r.URL.Path, "/library/sections/") ||
		r.URL.Path == "/security/token"
	if (library && tok != accountToken) || (!library && tok != transient) {
		http.Error(w, "unauthorized", 401)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/security/token":
		io.WriteString(w, `<MediaContainer token="`+transient+`"/>`)
	case p == "/library/sections":
		io.WriteString(w, `<MediaContainer><Directory key="1" type="movie" title="Films"/><Directory key="2" type="show" title="TV"/><Directory key="3" type="artist" title="Music"/></MediaContainer>`)
	case p == "/library/sections/1/all":
		io.WriteString(w, `<MediaContainer>`+
			`<Video ratingKey="10" key="/library/metadata/10" type="movie" title="Inception" year="2010"><Media><Part/></Media></Video>`+
			`<Video ratingKey="11" key="/library/metadata/11" type="movie" title="Dune" year="1984"/>`+
			`<Video ratingKey="12" key="/library/metadata/12" type="movie" title="Dune" year="2021"/>`+
			`<Video ratingKey="13" key="/library/metadata/13" type="movie" title="Blade Runner" year="1982"/>`+
			`<Video ratingKey="14" key="/library/metadata/14" type="movie" title="Blade Runner 2049" year="2017"/>`+
			`<Video ratingKey="15" key="/library/metadata/15" type="movie" title="Rocky II" year="1979"/>`+
			`<Video ratingKey="16" key="/library/metadata/16" type="movie" title="Fargo" year="1996"/>`+
			`<Video ratingKey="17" key="/library/metadata/17" type="movie" title="Amélie" year="2001"/>`+
			extra+`</MediaContainer>`)
	case p == "/library/sections/2/all":
		io.WriteString(w, `<MediaContainer>`+
			`<Directory ratingKey="20" key="/library/metadata/20/children" type="show" title="The Office" year="2005"/>`+
			`<Directory ratingKey="21" key="/library/metadata/21/children" type="show" title="Doctor Who (2005)" year="2005"/>`+
			`<Directory ratingKey="22" key="/library/metadata/22/children" type="show" title="Fargo" year="2014"/>`+
			`</MediaContainer>`)
	case strings.HasSuffix(p, "/allLeaves"):
		show := strings.TrimSuffix(strings.TrimPrefix(p, "/library/metadata/"), "/allLeaves")
		var b strings.Builder
		b.WriteString(`<MediaContainer>`)
		for _, e := range episodes {
			if e.show == show {
				b.WriteString(e.xml())
			}
		}
		b.WriteString(`</MediaContainer>`)
		io.WriteString(w, b.String())
	case strings.HasPrefix(p, "/library/metadata/"):
		rk := strings.TrimPrefix(p, "/library/metadata/")
		for _, e := range episodes {
			if e.rk == rk {
				io.WriteString(w, `<MediaContainer>`+e.xml()+`</MediaContainer>`)
				return
			}
		}
		title := map[string]string{"10": "Inception", "11": "Dune", "12": "Dune", "13": "Blade Runner", "14": "Blade Runner 2049", "15": "Rocky II", "16": "Fargo", "17": "Amélie", "18": "Arrival"}[rk]
		if title == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<MediaContainer><Video ratingKey="%s" key="/library/metadata/%s" type="movie" title="%s" duration="7200000" viewOffset="%d"><Media container="mkv" videoCodec="hevc" audioCodec="eac3"><Part id="1%s" key="/library/parts/1%s/file.mkv"><Stream id="1" streamType="1" codec="hevc"/><Stream id="2" streamType="2" codec="eac3" selected="1"/></Part></Media></Video></MediaContainer>`, rk, rk, title, offset.Milliseconds(), rk, rk)
	case p == "/video/:/transcode/universal/decision":
		time.Sleep(slow)
		io.WriteString(w, `<MediaContainer generalDecisionCode="1000"/>`)
	default:
		io.WriteString(w, `<MediaContainer/>`)
	}
}

func (e episode) xml() string {
	attrs := ""
	if e.watched {
		attrs += ` viewCount="1"`
	}
	if e.offset > 0 {
		attrs += fmt.Sprintf(` viewOffset="%d"`, e.offset)
	}
	return fmt.Sprintf(`<Video ratingKey="%s" key="/library/metadata/%s" type="episode" title="Episode %d" grandparentTitle="%s" grandparentRatingKey="%s" parentIndex="%d" index="%d" duration="1320000"%s><Media container="mp4" videoCodec="h264" audioCodec="aac"><Part id="9%s" key="/library/parts/9%s/file.mp4"/></Media></Video>`,
		e.rk, e.rk, e.index, xmlEscape(e.showTitle), e.show, e.season, e.index, attrs, e.rk, e.rk)
}

func xmlEscape(s string) string { return strings.NewReplacer("&", "&amp;", `"`, "&quot;").Replace(s) }

func (f *fakePMS) saw(path string) []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*http.Request
	for _, r := range f.requests {
		if r.URL.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// device is the device as the daemon's server drives it: what it plays, and what its player is doing
// with the add-on's latest, which the server tells the add-on as media.state.
type device struct {
	srv *serve.Server

	mu     sync.Mutex
	media  []addonapi.MediaPlay
	ops    []string
	refuse error // what the next media.play answers
	next   int
	cur    addonapi.MediaState

	entities map[string]any
}

func (d *device) Info() addonapi.Device {
	return addonapi.Device{Family: "show", Width: 960, Height: 480, Language: "en", Version: "test"}
}
func (d *device) Settings(string) map[string]any                           { return nil }
func (d *device) Connected() bool                                          { return true }
func (d *device) Quiet() bool                                              { return false }
func (d *device) Say(context.Context, string, string) error                { return nil }
func (d *device) PlaySound(string, string, *addonapi.WAV) error            { return nil }
func (d *device) StartTimer(string, string, time.Duration) (string, error) { return "local:1", nil }
func (d *device) CancelTimer(string, string) bool                          { return false }
func (d *device) Timers(string) []addonapi.Timer                           { return nil }
func (d *device) SetAwake(bool)                                            {}
func (d *device) Fire(string, string, map[string]string) error             { return nil }
func (d *device) SetEntity(_ string, e addonapi.Entity, state any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entities == nil {
		d.entities = map[string]any{}
	}
	d.entities[e.ID] = state
	return nil
}

func (d *device) entity(id string) any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entities[id]
}

func (d *device) MediaStatus(string) addonapi.MediaState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur.Media == "" {
		return addonapi.MediaState{State: addonapi.MediaIdle}
	}
	return d.cur
}

func ms(d time.Duration) *int64 {
	v := d.Milliseconds()
	return &v
}

func (d *device) active() bool {
	return d.cur.Media != "" && d.cur.State != addonapi.MediaStopped
}

func (d *device) Media(id, op string, p addonapi.MediaPlay) (string, error) {
	d.mu.Lock()
	d.ops = append(d.ops, op)
	if op == addonapi.OpMediaPlay {
		if d.refuse != nil {
			err := d.refuse
			d.refuse = nil
			d.mu.Unlock()
			return "", err
		}
		if d.active() {
			// The add-on's next play replaces its last, whose end it hears first.
			old := d.cur
			old.State, old.Reason = addonapi.MediaStopped, addonapi.MediaReplaced
			d.srv.MediaEnded(addonID, old)
		}
		d.next++
		d.media = append(d.media, p)
		d.cur = addonapi.MediaState{Media: strconv.Itoa(d.next), Kind: p.Kind, State: addonapi.MediaBuffering, PositionMs: ms(0)}
		mid := d.cur.Media
		d.mu.Unlock()
		d.srv.MediaChanged(addonID)
		return mid, nil
	}
	if !d.active() {
		d.mu.Unlock()
		return "", addonapi.Errorf(addonapi.CodeBadRequest, "nothing the add-on started is playing")
	}
	switch op {
	case addonapi.OpMediaPause:
		d.cur.State = addonapi.MediaPaused
	case addonapi.OpMediaResume:
		d.cur.State = addonapi.MediaPlaying
	case addonapi.OpMediaStop:
		d.cur.State, d.cur.Reason = addonapi.MediaStopped, addonapi.MediaStoppedByAddon
		end := d.cur
		d.mu.Unlock()
		d.srv.MediaEnded(addonID, end)
		return "", nil
	}
	d.mu.Unlock()
	d.srv.MediaChanged(addonID)
	return "", nil
}

// set is the device's player moving the add-on's media to state at pos, as the device would say.
func (d *device) set(state string, pos time.Duration) {
	d.mu.Lock()
	d.cur.State, d.cur.PositionMs = state, ms(pos)
	d.mu.Unlock()
	d.srv.MediaChanged(addonID)
}

// end is the add-on's media over on the device, for reason, at pos.
func (d *device) end(reason string, pos time.Duration) {
	d.mu.Lock()
	d.cur.State, d.cur.Reason, d.cur.PositionMs = addonapi.MediaStopped, reason, ms(pos)
	end := d.cur
	d.mu.Unlock()
	d.srv.MediaEnded(addonID, end)
}

func (d *device) last() addonapi.MediaPlay {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.media) == 0 {
		return addonapi.MediaPlay{}
	}
	return d.media[len(d.media)-1]
}

func (d *device) played() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.media)
}

// manifest is addon.json as the device grants it.
type manifest struct {
	ID           string             `json:"id"`
	API          int                `json:"api"`
	Capabilities []string           `json:"capabilities"`
	Commands     []addonapi.Command `json:"commands"`
	Entities     []addonapi.Entity  `json:"entities"`
}

func readManifest(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile("../../addon.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if err := addonapi.ValidCommands(m.Commands); err != nil {
		t.Fatal(err)
	}
	return m
}

type rig struct {
	pms    *fakePMS
	dev    *device
	srv    *serve.Server
	v      *Voice
	linked string

	tmu    sync.Mutex
	traced []addonapi.Request
}

// newRig is the add-on's voice connected to the daemon's own server for the API, with addon.json's
// grant, against a fake Plex server.
func newRig(t *testing.T) *rig {
	t.Helper()
	m := readManifest(t)
	if m.ID != addonID || m.API != 1 {
		t.Fatalf("addon.json: id %q, api %d", m.ID, m.API)
	}
	g := &rig{pms: newPMS(t), dev: &device{}, linked: accountToken}
	dir, err := os.MkdirTemp("", "t5plex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	uid := uint32(os.Getuid())
	if uid == 0 {
		t.Skip("the API refuses root")
	}
	g.srv = serve.New(dir, g.dev, func(u uint32) bool { return u == uid })
	g.dev.srv = g.srv
	g.srv.Chown = func(string, int) error { return nil }
	// Half a minute goes by between requests, so the tests are not held to media.play's rate (3, then
	// one every 30 seconds), which is the daemon's to keep.
	var clock sync.Mutex
	now := time.Now()
	g.srv.Now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		now = now.Add(31 * time.Second)
		return now
	}
	g.srv.Trace = func(_ string, req addonapi.Request, _ error) {
		g.tmu.Lock()
		g.traced = append(g.traced, req)
		g.tmu.Unlock()
	}
	path, err := g.srv.Open(serve.Grant{ID: addonID, UID: uid, Caps: m.Capabilities, Commands: m.Commands, Entities: m.Entities})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.srv.Close(addonID) })

	p := companion.New(plextv.NewIdentity("player-1", "Kitchen", "0.3.0", ""), func() companion.Renderer { return nil })
	p.Fetch = &http.Client{Timeout: 5 * time.Second} // the stand-in server is on loopback
	p.RefuseAddr = func(net.IP) bool { return false }
	base := g.pms.srv.URL
	servers := func(_ context.Context, token string) ([]plextv.Server, error) {
		if token != accountToken {
			return nil, plextv.StatusError{Code: 401}
		}
		return []plextv.Server{{Name: "Home", Machine: "server-1", Token: accountToken, Owned: true, Bases: []string{base}}}, nil
	}
	media := &Media{}
	g.v = New(p, media, func() string { return g.token() }, servers, &http.Client{Timeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go g.v.Run(ctx)
	go g.v.Serve(ctx, path, media)
	// Connected, and taking commands, once it has asked what its media is doing: it asks that first.
	g.waitOp(t, addonapi.OpMediaStatus, 1)
	return g
}

// waitOp waits until the add-on has sent op n times.
func (g *rig) waitOp(t *testing.T, op string, n int) {
	t.Helper()
	eventually(t, "the add-on did not send "+op, func() bool {
		seen := 0
		for _, r := range g.requests() {
			if r.Op == op {
				seen++
			}
		}
		return seen >= n
	})
}

func (g *rig) token() string {
	g.tmu.Lock()
	defer g.tmu.Unlock()
	return g.linked
}

// say calls a command as Home Assistant's addon_command does, and answers what it says back.
func (g *rig) say(t *testing.T, command string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	text, err := g.srv.Command(ctx, addonID, command, args, "action", true)
	if err != nil {
		t.Fatalf("%s %v: %v", command, args, err)
	}
	return text
}

func (g *rig) requests() []addonapi.Request {
	g.tmu.Lock()
	defer g.tmu.Unlock()
	return append([]addonapi.Request(nil), g.traced...)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPlayAFilm(t *testing.T) {
	g := newRig(t)
	if got := g.say(t, CmdPlay, map[string]any{"title": "inception"}); got != "Playing Inception." {
		t.Fatalf("answer %q", got)
	}
	mp := g.dev.last()
	if mp.Kind != addonapi.MediaVideo || mp.Title != "Inception" {
		t.Fatalf("played %+v", mp)
	}
	u, err := url.Parse(mp.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/video/:/transcode/universal/start" || "http://"+u.Host != g.pms.srv.URL {
		t.Fatalf("the device was handed %s%s", u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("X-Plex-Token") != transientToken || strings.Contains(mp.URL, accountToken) {
		t.Fatal("the stream's address does not carry the transient token alone")
	}
	if q.Get("path") != "/library/metadata/10" || q.Get("offset") != "0" {
		t.Fatalf("the stream is of %s from %s", q.Get("path"), q.Get("offset"))
	}
	// The server is told it plays, as for a cast, once the device says it does.
	g.dev.set(addonapi.MediaPlaying, 0)
	eventually(t, "no timeline report", func() bool {
		for _, r := range g.pms.saw("/:/timeline") {
			if r.URL.Query().Get("state") == "playing" && r.URL.Query().Get("ratingKey") == "10" {
				return true
			}
		}
		return false
	})
}

// The decision is the cast path's: the server is asked for the device's profile (H.264 with AAC, MP3 or
// AC-3, no bigger than the device's picture), copying what fits (directStream) and converting the rest,
// and the device is handed the continuous stream that decision is for.
func TestTheStreamIsDecidedForTheDevice(t *testing.T) {
	g := newRig(t)
	g.v.Player.VideoW, g.v.Player.VideoH = 1280, 720
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	ds := g.pms.saw("/video/:/transcode/universal/decision")
	if len(ds) != 1 {
		t.Fatalf("%d decisions", len(ds))
	}
	dq := ds[0].URL.Query()
	extra := dq.Get("X-Plex-Client-Profile-Extra")
	for _, want := range []string{"videoCodec=h264", "audioCodec=aac", "name=video.height&value=720", "name=video.width&value=1280"} {
		if !strings.Contains(extra, want) {
			t.Errorf("the profile lacks %s: %s", want, extra)
		}
	}
	if dq.Get("directStream") != "1" || dq.Get("directPlay") != "0" {
		t.Errorf("directStream %q directPlay %q", dq.Get("directStream"), dq.Get("directPlay"))
	}
	sq, _ := url.Parse(g.dev.last().URL)
	if sq.Query().Get("session") != dq.Get("session") || dq.Get("session") == "" {
		t.Error("the stream is not the decision's session")
	}
}

func TestPlayAShowGoesOnWhereItWasLeft(t *testing.T) {
	g := newRig(t)
	// S1E1 and S1E2 watched, a special not: the next is S2E1.
	if got := g.say(t, CmdPlay, map[string]any{"title": "the office"}); got != "Playing The Office, season 2 episode 1." {
		t.Fatalf("answer %q", got)
	}
	if mp := g.dev.last(); mp.Title != "The Office: S2 E1 Episode 1" {
		t.Fatalf("title %q", mp.Title)
	}
	// One started and not finished is the one asked about.
	if got := g.say(t, CmdPlay, map[string]any{"title": "fargo"}); got != "Fargo (2014), season 1 episode 2 is 10 minutes in. Resume, or start from the beginning?" {
		t.Fatalf("answer %q", got)
	}
	if got := g.say(t, CmdChoose, map[string]any{"answer": "resume"}); got != "Resuming Fargo (2014), season 1 episode 2 at 10 minutes." {
		t.Fatalf("answer %q", got)
	}
	u, _ := url.Parse(g.dev.last().URL)
	if u.Query().Get("offset") != "600" {
		t.Fatalf("offset %q", u.Query().Get("offset"))
	}
}

func TestAShowAllWatchedStartsAgain(t *testing.T) {
	g := newRig(t)
	if got := g.say(t, CmdPlay, map[string]any{"title": "doctor who"}); got != "Playing Doctor Who (2005), season 1 episode 1." {
		t.Fatalf("answer %q", got)
	}
}

// Stoney's lesson: a title with its year in it never matched what is said.
func TestATitleWithAYearMatches(t *testing.T) {
	g := newRig(t)
	for _, said := range []string{"doctor who", "Doctor Who", "the doctor who", "doctor who 2005", "Doctor Who (2005)"} {
		got := g.say(t, CmdPlay, map[string]any{"title": said})
		if !strings.HasPrefix(got, "Playing Doctor Who") {
			t.Errorf("%q: %q", said, got)
		}
	}
}

func TestPlayAnEpisode(t *testing.T) {
	g := newRig(t)
	got := g.say(t, CmdPlayEpisode, map[string]any{"title": "office", "season": "2", "episode": "four"})
	if got != "Playing The Office, season 2 episode 4." {
		t.Fatalf("answer %q", got)
	}
	u, _ := url.Parse(g.dev.last().URL)
	if u.Query().Get("path") != "/library/metadata/206" {
		t.Fatalf("played %s", u.Query().Get("path"))
	}
	// The numbers said as part of the title play the episode too.
	if got := g.say(t, CmdPlay, map[string]any{"title": "the office season two episode 4"}); got != "Playing The Office, season 2 episode 4." {
		t.Fatalf("play with the numbers in the title: %q", got)
	}
	if got := g.say(t, CmdPlayEpisode, map[string]any{"title": "The Office", "season": "9", "episode": "1"}); got != "The Office has no season 9 episode 1 in your Plex library." {
		t.Fatalf("answer %q", got)
	}
	// Only shows have episodes: a film of the name is not taken.
	if got := g.say(t, CmdPlayEpisode, map[string]any{"title": "Inception", "season": "1", "episode": "1"}); !strings.HasPrefix(got, "I couldn't find") {
		t.Fatalf("answer %q", got)
	}
	if got := g.say(t, CmdPlayEpisode, map[string]any{"title": "The Office", "season": "x", "episode": "1"}); !strings.HasPrefix(got, "Which season") {
		t.Fatalf("answer %q", got)
	}
}

// A server that restarted has forgotten its transient token: the command asks for a new one and plays.
func TestAStaleTransientTokenIsReplaced(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	g.pms.mu.Lock()
	g.pms.tok = "transient-secret-token-3"
	g.pms.mu.Unlock()
	if got := g.say(t, CmdPlay, map[string]any{"title": "the office"}); got != "Playing The Office, season 2 episode 1." {
		t.Fatalf("answer %q", got)
	}
	if u, _ := url.Parse(g.dev.last().URL); u.Query().Get("X-Plex-Token") != "transient-secret-token-3" {
		t.Fatal("the stream is not on the new token")
	}
}

// A film part watched is not started: the add-on asks, the asking sensor is on until it is answered,
// and the answer (with filler, or "resume" itself) resumes it or starts it over.
func TestAskToResumeOrStartOver(t *testing.T) {
	g := newRig(t)
	g.pms.mu.Lock()
	g.pms.offset = 70*time.Minute + 20*time.Second
	g.pms.mu.Unlock()
	const asked = "Inception is 1 hour 10 minutes in. Resume, or start from the beginning?"
	offset := func() string {
		u, _ := url.Parse(g.dev.last().URL)
		return u.Query().Get("offset")
	}
	asking := func(want bool) {
		t.Helper()
		eventually(t, fmt.Sprintf("asking is not %v", want), func() bool { return g.dev.entity(EntityAsking) == want })
	}
	if got := g.say(t, CmdChoose, map[string]any{"answer": "resume"}); got != "There's nothing to choose." {
		t.Fatalf("choose with nothing asked: %q", got)
	}
	for _, c := range []struct {
		answer, want, offset string
	}{
		{"resume", "Resuming Inception at 1 hour 10 minutes.", "4220"},
		{"um, keep going please", "Resuming Inception at 1 hour 10 minutes.", "4220"},
		{"where I left off", "Resuming Inception at 1 hour 10 minutes.", "4220"},
		{"and from the beginning", "Playing Inception.", "0"},
		{"uh start it over", "Playing Inception.", "0"},
		{"restart", "Playing Inception.", "0"},
	} {
		if got := g.say(t, CmdPlay, map[string]any{"title": "inception"}); got != asked {
			t.Fatalf("answer %q", got)
		}
		asking(true)
		plays := g.dev.played()
		if got := g.say(t, CmdChoose, map[string]any{"answer": c.answer}); got != c.want {
			t.Errorf("%q: %q", c.answer, got)
		}
		if g.dev.played() != plays+1 || offset() != c.offset {
			t.Errorf("%q: played from %s", c.answer, offset())
		}
		asking(false)
	}
	// An answer that is neither asks again, and the question still waits.
	g.say(t, CmdPlay, map[string]any{"title": "inception"})
	if got := g.say(t, CmdChoose, map[string]any{"answer": "banana"}); got != "Resume, or start from the beginning?" {
		t.Fatalf("answer %q", got)
	}
	// resume, with nothing paused, answers the question.
	if got := g.say(t, CmdResume, nil); got != "Resuming Inception at 1 hour 10 minutes." {
		t.Fatalf("resume: %q", got)
	}
	// from_start, or a title that asks for the beginning, plays at once.
	for _, args := range []map[string]any{
		{"title": "inception", "from_start": true},
		{"title": "Inception from the beginning"},
	} {
		if got := g.say(t, CmdPlay, args); got != "Playing Inception." || offset() != "0" {
			t.Errorf("%v: %q from %s", args, got, offset())
		}
	}
	if got := g.say(t, CmdRestart, nil); got != "Starting Inception from the beginning." || offset() != "0" {
		t.Fatalf("restart: %q", got)
	}
	// An episode by its numbers asks too.
	if got := g.say(t, CmdPlayEpisode, map[string]any{"title": "fargo", "season": "1", "episode": "2"}); got != "Fargo, season 1 episode 2 is 10 minutes in. Resume, or start from the beginning?" {
		t.Fatalf("answer %q", got)
	}
	if got := g.say(t, CmdChoose, map[string]any{"answer": "the beginning"}); got != "Playing Fargo, season 1 episode 2." {
		t.Fatalf("answer %q", got)
	}
	g.say(t, CmdStop, nil)
	if got := g.say(t, CmdRestart, nil); got != "Starting Fargo, season 1 episode 2 from the beginning." {
		t.Fatalf("restart after stop: %q", got)
	}
}

// A question not answered is dropped after AskFor, and the asking sensor goes off.
func TestAQuestionLapses(t *testing.T) {
	old := AskFor
	AskFor = 200 * time.Millisecond
	t.Cleanup(func() { AskFor = old })
	g := newRig(t)
	g.pms.mu.Lock()
	g.pms.offset = 5 * time.Minute
	g.pms.mu.Unlock()
	g.say(t, CmdPlay, map[string]any{"title": "inception"})
	eventually(t, "asking never on", func() bool { return g.dev.entity(EntityAsking) == true })
	eventually(t, "asking never off", func() bool { return g.dev.entity(EntityAsking) == false })
	if got := g.say(t, CmdChoose, map[string]any{"answer": "resume"}); got != "There's nothing to choose." {
		t.Fatalf("answer %q", got)
	}
	if g.dev.played() != 0 {
		t.Fatal("something was played")
	}
}

// Stop while a question waits drops it: the asking sensor goes off and nothing is started.
func TestStopDropsTheQuestion(t *testing.T) {
	g := newRig(t)
	g.pms.mu.Lock()
	g.pms.offset = 5 * time.Minute
	g.pms.mu.Unlock()
	g.say(t, CmdPlay, map[string]any{"title": "inception"})
	eventually(t, "asking never on", func() bool { return g.dev.entity(EntityAsking) == true })
	if got := g.say(t, CmdStop, nil); got != "OK, I won't start it." {
		t.Fatalf("stop: %q", got)
	}
	eventually(t, "asking never off", func() bool { return g.dev.entity(EntityAsking) == false })
	if got := g.say(t, CmdChoose, map[string]any{"answer": "resume"}); got != "There's nothing to choose." {
		t.Fatalf("answer %q", got)
	}
	if g.dev.played() != 0 {
		t.Fatal("something was played")
	}
	// With nothing asked, stop says what it always did.
	if got := g.say(t, CmdStop, nil); got != "Nothing from Plex is playing." {
		t.Fatalf("stop: %q", got)
	}
}

// The paused sensor is on while what voice plays is paused, and set again on a new connection.
func TestThePausedSensor(t *testing.T) {
	g := newRig(t)
	eventually(t, "the sensors were not set on connecting", func() bool {
		return g.dev.entity(EntityPaused) == false && g.dev.entity(EntityAsking) == false
	})
	g.say(t, CmdPlay, map[string]any{"title": "dune 2021"})
	g.dev.set(addonapi.MediaPlaying, time.Second)
	g.waitReport(t, "playing", "")
	g.say(t, CmdPause, nil)
	eventually(t, "paused not on", func() bool { return g.dev.entity(EntityPaused) == true })
	g.say(t, CmdResume, nil)
	eventually(t, "paused not off on resume", func() bool { return g.dev.entity(EntityPaused) == false })
	g.say(t, CmdPause, nil)
	eventually(t, "paused not on", func() bool { return g.dev.entity(EntityPaused) == true })
	g.dev.end(addonapi.MediaStoppedOnDevice, 10*time.Second)
	eventually(t, "paused not off when stopped", func() bool { return g.dev.entity(EntityPaused) == false })
}

func TestRestartWithNothing(t *testing.T) {
	g := newRig(t)
	if got := g.say(t, CmdRestart, nil); got != "There's nothing to start over." {
		t.Fatalf("answer %q", got)
	}
}

func TestNotFound(t *testing.T) {
	g := newRig(t)
	if got := g.say(t, CmdPlay, map[string]any{"title": "Zardoz"}); got != "I couldn't find Zardoz in your Plex library." {
		t.Fatalf("answer %q", got)
	}
	if g.dev.played() != 0 {
		t.Fatal("something was played")
	}
}

// A film added since the library was read is found: a title not in a list older than a couple of
// minutes has it read again.
func TestANewFilmIsFound(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	g.pms.mu.Lock()
	g.pms.extra = `<Video ratingKey="18" key="/library/metadata/18" type="movie" title="Arrival" year="2016"/>`
	g.pms.mu.Unlock()
	g.v.lib.mu.Lock()
	g.v.lib.at = time.Now().Add(-staleAfter - time.Second)
	g.v.lib.mu.Unlock()
	if got := g.say(t, CmdPlay, map[string]any{"title": "arrival"}); got != "Playing Arrival." {
		t.Fatalf("answer %q", got)
	}
}

func TestTwoOfATitleAreToldApartByTheYear(t *testing.T) {
	g := newRig(t)
	for said, want := range map[string]string{
		"dune":        "Playing Dune (2021).",
		"dune 1984":   "Playing Dune (1984).",
		"Dune (1984)": "Playing Dune (1984).",
		"fargo":       "Fargo (2014), season 1 episode 2 is 10 minutes in. Resume, or start from the beginning?",
	} {
		got := g.say(t, CmdPlay, map[string]any{"title": said})
		if got != want {
			t.Errorf("%q: %q, want %q", said, got, want)
		}
	}
	// "blade runner 2049" is the film of that name, not Blade Runner of the year 2049.
	g.say(t, CmdPlay, map[string]any{"title": "blade runner 2049"})
	if u, _ := url.Parse(g.dev.last().URL); u.Query().Get("path") != "/library/metadata/14" {
		t.Errorf("blade runner 2049 played %s", u.Query().Get("path"))
	}
}

func TestPauseResumeStop(t *testing.T) {
	g := newRig(t)
	if got := g.say(t, CmdPause, nil); got != sayNothing {
		t.Fatalf("pause with nothing: %q", got)
	}
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	g.dev.set(addonapi.MediaPlaying, 95*time.Second)
	g.waitReport(t, "playing", "95000")
	for _, c := range []struct{ cmd, want, state string }{
		{CmdPause, sayPaused, "paused"},
		{CmdResume, sayResumed, "playing"},
	} {
		if got := g.say(t, c.cmd, nil); got != c.want {
			t.Fatalf("%s: %q", c.cmd, got)
		}
		g.waitReport(t, c.state, "")
	}
	g.dev.set(addonapi.MediaPlaying, 300*time.Second)
	if got := g.say(t, CmdStop, nil); got != sayStopped {
		t.Fatalf("stop: %q", got)
	}
	// Reported stopped where the device says it stopped.
	g.waitReport(t, "stopped", "300000")
	eventually(t, "the stream was not ended", func() bool { return len(g.pms.saw("/video/:/transcode/universal/stop")) == 1 })
	if got := g.say(t, CmdResume, nil); got != sayNothing {
		t.Fatalf("resume after stop: %q", got)
	}
}

// waitReport waits for the server's last report to be state, at at (milliseconds; "" for any).
func (g *rig) waitReport(t *testing.T, state, at string) {
	t.Helper()
	defer func() {
		if t.Failed() {
			for _, r := range g.pms.saw("/:/timeline") {
				t.Log("report", r.URL.Query().Get("ratingKey"), r.URL.Query().Get("state"), r.URL.Query().Get("time"))
			}
		}
	}()
	eventually(t, "no "+state+" report at "+at, func() bool {
		rs := g.pms.saw("/:/timeline")
		if len(rs) == 0 {
			return false
		}
		q := rs[len(rs)-1].URL.Query()
		return q.Get("state") == state && (at == "" || q.Get("time") == at)
	})
}

// Stopped on the device, with its Stop button: reported stopped where it was, so the server's own rule
// says whether it was watched, and nothing is left to pause.
func TestStoppedOnTheDevice(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	g.dev.set(addonapi.MediaPlaying, 0)
	g.dev.end(addonapi.MediaStoppedOnDevice, 10*time.Minute)
	g.waitReport(t, "stopped", "600000")
	eventually(t, "the stream was not ended", func() bool { return len(g.pms.saw("/video/:/transcode/universal/stop")) == 1 })
	if got := g.say(t, CmdPause, nil); got != sayNothing {
		t.Fatalf("answer %q", got)
	}
}

// Played to its end: reported stopped at its end, which marks it watched.
func TestPlayedToItsEndIsWatched(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	g.dev.set(addonapi.MediaPlaying, 0)
	g.dev.end(addonapi.MediaEnded, 7190*time.Second)
	g.waitReport(t, "stopped", "7200000")
}

// A play that replaces voice's last: the last is reported stopped where it was, then the new one plays.
func TestReplacedIsReportedWhereItWas(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	g.dev.set(addonapi.MediaPlaying, 42*time.Second)
	g.waitReport(t, "playing", "42000")
	g.say(t, CmdPlay, map[string]any{"title": "Dune 2021"})
	eventually(t, "Inception not reported stopped at 42 s", func() bool {
		for _, r := range g.pms.saw("/:/timeline") {
			q := r.URL.Query()
			if q.Get("ratingKey") == "10" && q.Get("state") == "stopped" && q.Get("time") == "42000" {
				return true
			}
		}
		return false
	})
}

// Something else on the device's screen since, its end not heard: the daemon refuses a pause of what
// is not the add-on's, and it is taken as over.
func TestWhatIsGoneIsNotPaused(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	g.dev.mu.Lock()
	g.dev.cur.State = addonapi.MediaStopped // without a word to the add-on
	g.dev.mu.Unlock()
	if got := g.say(t, CmdPause, nil); got != sayNothing {
		t.Fatalf("answer %q", got)
	}
	g.waitReport(t, "stopped", "")
}

// Connected again after the daemon was away: media.status says how what voice played ended meanwhile.
func TestMediaStatusAfterConnectingAgain(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Inception"})
	g.dev.set(addonapi.MediaPlaying, 0)
	g.waitReport(t, "playing", "0")
	g.srv.Close(addonID)
	g.dev.mu.Lock()
	g.dev.cur.State, g.dev.cur.Reason, g.dev.cur.PositionMs = addonapi.MediaStopped, addonapi.MediaStoppedOnDevice, ms(100*time.Second)
	g.dev.mu.Unlock()
	uid := uint32(os.Getuid())
	m := readManifest(t)
	if _, err := g.srv.Open(serve.Grant{ID: addonID, UID: uid, Caps: m.Capabilities, Commands: m.Commands, Entities: m.Entities}); err != nil {
		t.Fatal(err)
	}
	g.waitReport(t, "stopped", "100000")
}

func TestNextEpisode(t *testing.T) {
	g := newRig(t)
	if got := g.say(t, CmdNextEpisode, nil); got != sayNothing {
		t.Fatalf("with nothing: %q", got)
	}
	g.say(t, CmdPlayEpisode, map[string]any{"title": "the office", "season": "2", "episode": "3"})
	if got := g.say(t, CmdNextEpisode, nil); got != "Playing The Office, season 2 episode 4." {
		t.Fatalf("answer %q", got)
	}
	if got := g.say(t, CmdNextEpisode, nil); got != "That was the last episode of The Office." {
		t.Fatalf("answer %q", got)
	}
	g.say(t, CmdPlay, map[string]any{"title": "inception"})
	if got := g.say(t, CmdNextEpisode, nil); got != "What's playing isn't an episode." {
		t.Fatalf("answer %q", got)
	}
}

func TestNotLinked(t *testing.T) {
	g := newRig(t)
	g.tmu.Lock()
	g.linked = ""
	g.tmu.Unlock()
	if got := g.say(t, CmdPlay, map[string]any{"title": "Inception"}); got != sayNotLinked {
		t.Fatalf("answer %q", got)
	}
}

func TestTheDeviceRefuses(t *testing.T) {
	g := newRig(t)
	g.dev.mu.Lock()
	g.dev.refuse = addonapi.Errorf(addonapi.CodeUnavailable, "video is off")
	g.dev.mu.Unlock()
	if got := g.say(t, CmdPlay, map[string]any{"title": "Inception"}); got != "This device can't play Plex video right now. video is off" {
		t.Fatalf("answer %q", got)
	}
	eventually(t, "the refused stream was not ended", func() bool { return len(g.pms.saw("/video/:/transcode/universal/stop")) == 1 })
}

// A server slow to decide: Home Assistant is answered in time, and the film starts anyway.
func TestASlowServerIsAnsweredInTime(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Dune"}) // the library read, the token got
	g.pms.mu.Lock()
	g.pms.slow = ReplyWithin + 500*time.Millisecond
	g.pms.mu.Unlock()
	began := time.Now()
	if got := g.say(t, CmdPlay, map[string]any{"title": "Inception"}); got != "Looking for Inception on Plex." {
		t.Fatalf("answer %q", got)
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Fatalf("answered after %v", took)
	}
	eventually(t, "it did not start", func() bool { return g.dev.last().Title == "Inception" })
}

// No token in the log, at debug level and with nothing to redact it, and nothing of a token sent to
// the daemon except in the address the device's player fetches.
func TestNoTokenInTheLogOrTheDaemon(t *testing.T) {
	var buf safeBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "the office"})
	g.say(t, CmdPause, nil)
	g.say(t, CmdResume, nil)
	g.say(t, CmdNextEpisode, nil)
	g.say(t, CmdPlay, map[string]any{"title": "nothing like it"})
	g.dev.mu.Lock()
	g.dev.refuse = addonapi.Errorf(addonapi.CodeBusy, "quiet hours")
	g.dev.mu.Unlock()
	g.say(t, CmdPlay, map[string]any{"title": "inception"})
	g.say(t, CmdStop, nil)
	g.pms.srv.Close() // and a server that is gone
	g.v.lib.mu.Lock()
	g.v.lib.at = time.Now().Add(-time.Hour)
	g.v.lib.mu.Unlock()
	g.say(t, CmdPlay, map[string]any{"title": "missing"})
	time.Sleep(100 * time.Millisecond)

	log := buf.String()
	if !strings.Contains(log, "voice command") {
		t.Fatalf("nothing was logged at debug level:\n%s", log)
	}
	for _, tok := range []string{accountToken, transientToken} {
		if strings.Contains(log, tok) {
			t.Errorf("the log has a token:\n%s", log)
		}
	}
	plays := 0
	for _, r := range g.requests() {
		args := string(r.Args)
		if r.Op == addonapi.OpMediaPlay {
			plays++
			var mp addonapi.MediaPlay
			if err := json.Unmarshal(r.Args, &mp); err != nil {
				t.Fatal(err)
			}
			args = strings.Replace(args, mp.URL, "", 1)
			if strings.Contains(mp.URL, accountToken) {
				t.Errorf("the account's token was handed to the device")
			}
		}
		if strings.Contains(args, accountToken) || strings.Contains(args, transientToken) {
			t.Errorf("%s carries a token outside its address: %s", r.Op, args)
		}
	}
	if plays < 3 {
		t.Fatalf("%d plays", plays)
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// play with if_known: a title the library has plays; anything else is answered with nothing at once
// and plays nothing, so Home Assistant can send it to music. The library is never read while it waits.
func TestPlayIfKnown(t *testing.T) {
	g := newRig(t)
	known := map[string]any{"title": "inception", "if_known": true}
	// Not read yet (as on a new link): not known, and read in the background.
	g.v.forget()
	if got := g.say(t, CmdPlay, known); got != "" {
		t.Fatalf("before the library was read: %q", got)
	}
	if g.dev.played() != 0 {
		t.Fatal("something was played before the library was read")
	}
	eventually(t, "the library was not read in the background", func() bool {
		_, at, _ := g.v.lib.snapshot()
		return !at.IsZero()
	})
	for _, title := range []string{"bohemian rhapsody", "incepshun", "dune part three", "johnny cash"} {
		if got := g.say(t, CmdPlay, map[string]any{"title": title, "if_known": true}); got != "" {
			t.Errorf("%q: %q", title, got)
		}
	}
	if g.dev.played() != 0 {
		t.Fatal("an unknown title played")
	}
	for _, title := range []string{"The Inception", "dune 2021", "rocky two", "inception from the beginning", "fargo season 1 episode 1"} {
		if !g.v.Known(title) {
			t.Errorf("%q is not known", title)
		}
	}
	if got := g.say(t, CmdPlay, known); got != "Playing Inception." {
		t.Fatalf("a known title: %q", got)
	}
	if g.dev.played() != 1 {
		t.Fatal("the known title did not play")
	}
}

// An episode said plainly is found in a library that numbers episodes with the season in front.
func TestEpisodeNumberedWithItsSeason(t *testing.T) {
	g := newRig(t)
	if got := g.say(t, CmdPlayEpisode, map[string]any{"title": "fargo", "season": "2", "episode": "1"}); got != "Playing Fargo, season 2 episode 201." {
		t.Fatalf("answer %q", got)
	}
	if got := g.say(t, CmdPlayEpisode, map[string]any{"title": "fargo", "season": "2", "episode": "201"}); got != "Playing Fargo, season 2 episode 201." {
		t.Fatalf("answer %q", got)
	}
	if got := g.say(t, CmdPlayEpisode, map[string]any{"title": "fargo", "season": "2", "episode": "5"}); got != "Fargo has no season 2 episode 5 in your Plex library." {
		t.Fatalf("answer %q", got)
	}
}

// A question in an answer that comes after Home Assistant stopped waiting was never heard, so it does
// not wait for an answer: a later "resume" is not taken as one.
func TestALateQuestionIsDropped(t *testing.T) {
	g := newRig(t)
	g.say(t, CmdPlay, map[string]any{"title": "Dune"}) // the library read, the token got
	g.say(t, CmdStop, nil)
	g.pms.mu.Lock()
	g.pms.offset = 5 * time.Minute
	g.pms.slowItem = ReplyWithin + 500*time.Millisecond
	g.pms.mu.Unlock()
	plays := g.dev.played()
	if got := g.say(t, CmdPlay, map[string]any{"title": "Inception"}); got != "Looking for Inception on Plex." {
		t.Fatalf("answer %q", got)
	}
	time.Sleep(time.Second) // the late answer, a question, arrives
	eventually(t, "the late question still waits", func() bool { return !g.v.asking() })
	if got := g.say(t, CmdChoose, map[string]any{"answer": "resume"}); got != "There's nothing to choose." {
		t.Fatalf("choose: %q", got)
	}
	if g.dev.played() != plays {
		t.Fatal("something was played")
	}
}
