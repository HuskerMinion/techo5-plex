package companion

import (
	"context"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/dlna"
	"github.com/HuskerMinion/techo5-plex/internal/dlna/fakerenderer"
	"github.com/HuskerMinion/techo5-plex/internal/plextv"
)

const token = "transient-token-123"

// fakePMS is a Plex Media Server with one song, one film and a queue of two songs. It records every
// request's query and whether it carried the token as a header.
type fakePMS struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
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
	f.mu.Unlock()
	if r.Header.Get("X-Plex-Token") != token {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch r.URL.Path {
	case "/library/metadata/1":
		io.WriteString(w, `<MediaContainer><Track ratingKey="1" key="/library/metadata/1" type="track" title="Song &amp; One" grandparentTitle="Band" parentTitle="Album" thumb="/library/metadata/10/thumb/1" duration="180000"><Media duration="180000" container="flac" audioCodec="flac"><Part id="11" key="/library/parts/11/file.flac" container="flac"/></Media></Track></MediaContainer>`)
	case "/library/metadata/2":
		io.WriteString(w, `<MediaContainer><Track ratingKey="2" key="/library/metadata/2" type="track" title="Song Two" grandparentTitle="Band" parentTitle="Album" duration="200000"><Media container="m4a" audioCodec="aac"><Part id="12" key="/library/parts/12/file.m4a"/></Media></Track></MediaContainer>`)
	case "/library/metadata/3":
		io.WriteString(w, `<MediaContainer><Video ratingKey="3" key="/library/metadata/3" type="movie" title="A Film" duration="5400000"><Media container="mkv" videoCodec="hevc" audioCodec="eac3"><Part id="13" key="/library/parts/13/file.mkv"><Stream id="100" streamType="1" codec="hevc"/><Stream id="101" streamType="2" codec="eac3" selected="1"/><Stream id="102" streamType="2" codec="aac"/><Stream id="103" streamType="3" codec="srt"/></Part></Media></Video></MediaContainer>`)
	case "/playQueues/7":
		io.WriteString(w, `<MediaContainer playQueueID="7"><Track ratingKey="1" key="/library/metadata/1"/><Track ratingKey="2" key="/library/metadata/2"/></MediaContainer>`)
	case "/video/:/transcode/universal/decision":
		io.WriteString(w, `<MediaContainer generalDecisionCode="1000"/>`)
	default:
		io.WriteString(w, `<MediaContainer/>`)
	}
}

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

func (f *fakePMS) hostPort() (string, string) {
	u, _ := url.Parse(f.srv.URL)
	return u.Hostname(), u.Port()
}

type rig struct {
	p   *Player
	h   http.Handler
	pms *fakePMS
	fr  *fakerenderer.Renderer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pms := newPMS(t)
	fr := fakerenderer.New()
	t.Cleanup(fr.Close)
	r, err := dlna.Discover(context.Background(), nil, fr.Description())
	if err != nil {
		t.Fatal(err)
	}
	p := New(plextv.NewIdentity("player-1", "Kitchen", "0.1.0", ""), func() Renderer { return r })
	// The stand-in server is on loopback, which the real player refuses to fetch from.
	p.Fetch, p.Push = &http.Client{Timeout: 5 * time.Second}, &http.Client{Timeout: 5 * time.Second}
	p.RefuseAddr = func(net.IP) bool { return false }
	// The linked account lists the stand-in server, at its address.
	sh, _ := pms.hostPort()
	p.ServerToken = func(_ context.Context, machine, host string) string {
		if machine == "server-1" && host == sh {
			return "account-token"
		}
		return ""
	}
	return &rig{p: p, h: p.Handler(), pms: pms, fr: fr}
}

func (g *rig) get(t *testing.T, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	r.RemoteAddr, r.Host = "192.168.1.5:40000", "192.168.1.9:32500"
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	return w
}

func (g *rig) playMedia(t *testing.T, key, extra string) *httptest.ResponseRecorder {
	host, port := g.pms.hostPort()
	q := url.Values{"protocol": {"http"}, "address": {host}, "port": {port}, "key": {key}, "token": {token},
		"machineIdentifier": {"server-1"}}
	return g.get(t, "/player/playback/playMedia?"+q.Encode()+extra)
}

func (g *rig) timeline(t *testing.T) string {
	w := g.get(t, "/player/timeline/poll?wait=0&commandID=4")
	if w.Code != 200 {
		t.Fatalf("poll: %d", w.Code)
	}
	return w.Body.String()
}

func TestResources(t *testing.T) {
	g := newRig(t)
	w := g.get(t, "/resources")
	var c struct {
		Player struct {
			Title    string `xml:"title,attr"`
			ID       string `xml:"machineIdentifier,attr"`
			Caps     string `xml:"protocolCapabilities,attr"`
			Protocol string `xml:"protocol,attr"`
		} `xml:"Player"`
	}
	if err := xml.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Player.Title != "Kitchen" || c.Player.ID != "player-1" || c.Player.Protocol != "plex" || c.Player.Caps != Capabilities {
		t.Errorf("%+v", c.Player)
	}
	if w.Header().Get("X-Plex-Client-Identifier") != "player-1" || w.Header().Get("Access-Control-Expose-Headers") == "" {
		t.Errorf("headers %v", w.Header())
	}
}

// Who may ask: the home network, at the device's address, for this player.
func TestWhoMayAsk(t *testing.T) {
	g := newRig(t)
	for name, mod := range map[string]func(*http.Request){
		"the internet": func(r *http.Request) { r.RemoteAddr = "8.8.8.8:1" },
		"a name":       func(r *http.Request) { r.Host = "rebound.example:32500" },
	} {
		r := httptest.NewRequest("GET", "/resources", nil)
		r.RemoteAddr, r.Host = "192.168.1.5:1", "192.168.1.9:32500"
		mod(r)
		w := httptest.NewRecorder()
		g.h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := g.get(t, "/resources", "X-Plex-Target-Client-Identifier", "someone-else"); w.Code != http.StatusNotFound {
		t.Errorf("another player's command: %d", w.Code)
	}
}

func TestABadRequestIsRefused(t *testing.T) {
	g := newRig(t)
	g.p.RefuseAddr = func(ip net.IP) bool { return ip.IsLoopback() }
	if w := g.playMedia(t, "/library/metadata/1", ""); w.Code != http.StatusBadRequest {
		t.Errorf("a loopback server: %d", w.Code)
	}
	if w := g.get(t, "/player/playback/playMedia?protocol=file&address=x&port=1&key=/a&machineIdentifier=m"); w.Code != http.StatusBadRequest {
		t.Errorf("file protocol: %d", w.Code)
	}
	for _, c := range []mediaRequest{
		{protocol: "http", address: "192.168.1.20/x", port: "32400", key: "/a", machine: "m"},
		{protocol: "http", address: "a@b", port: "32400", key: "/a", machine: "m"},
		{protocol: "http", address: "192.168.1.20", port: "0", key: "/a", machine: "m"},
		{protocol: "http", address: "192.168.1.20", port: "32400", key: "//evil/a", machine: "m"},
		{protocol: "http", address: "192.168.1.20", port: "32400", key: "/a", machine: ""},
	} {
		if c.check() == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

// A FLAC is handed to the renderer as it is, described as music; the token goes to the server as a
// header, is in the renderer's URL (it fetches with no headers), and is never in a timeline.
func TestPlayASong(t *testing.T) {
	g := newRig(t)
	w := g.playMedia(t, "/library/metadata/1", "&containerKey="+url.QueryEscape("/playQueues/7?own=1"))
	if w.Code != 200 {
		t.Fatalf("playMedia: %d %s", w.Code, w.Body.String())
	}
	for _, r := range g.pms.saw("/library/metadata/1") {
		if strings.Contains(r.URL.RawQuery, token) {
			t.Error("the token was in the metadata request's query")
		}
	}
	set := g.fr.Last("SetAVTransportURI")
	if set == nil || g.fr.Last("Play") == nil {
		t.Fatal("the renderer was not told")
	}
	if !strings.HasPrefix(set.Args["CurrentURI"], g.pms.srv.URL+"/library/parts/11/file.flac?X-Plex-Token=") {
		t.Errorf("URI %q", set.Args["CurrentURI"])
	}
	meta := set.Args["CurrentURIMetaData"]
	for _, want := range []string{"object.item.audioItem.musicTrack", "Song &amp; One", "Band", "Album", "audio/flac", "0:03:00", "/photo/:/transcode"} {
		if !strings.Contains(meta, want) {
			t.Errorf("metadata lacks %q: %s", want, meta)
		}
	}
	g.fr.Set(dlna.Playing, "0:00:42")
	g.p.tick(context.Background())
	tl := g.timeline(t)
	if strings.Contains(tl, token) || strings.Contains(strings.ToLower(tl), "token=") {
		t.Fatalf("a timeline carried the token: %s", tl)
	}
	var mc struct {
		Command   string `xml:"commandID,attr"`
		Timelines []struct {
			Type    string `xml:"type,attr"`
			State   string `xml:"state,attr"`
			Time    int64  `xml:"time,attr"`
			Rating  string `xml:"ratingKey,attr"`
			Machine string `xml:"machineIdentifier,attr"`
			Queue   string `xml:"playQueueID,attr"`
		} `xml:"Timeline"`
	}
	if err := xml.Unmarshal([]byte(tl), &mc); err != nil {
		t.Fatal(err)
	}
	if len(mc.Timelines) != 3 || mc.Command != "4" {
		t.Fatalf("timeline %s", tl)
	}
	music := mc.Timelines[0]
	if music.Type != "music" || music.State != "playing" || music.Rating != "1" || music.Machine != "server-1" || music.Queue != "7" ||
		music.Time < 42000 || music.Time > 44000 {
		t.Errorf("music timeline %+v", music)
	}
	if mc.Timelines[1].State != "stopped" {
		t.Errorf("video timeline %+v", mc.Timelines[1])
	}
}

// AAC is not something the renderer decodes: the server is asked for MP3.
func TestASongItCannotDecodeIsTranscoded(t *testing.T) {
	g := newRig(t)
	if w := g.playMedia(t, "/library/metadata/2", ""); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	u, _ := url.Parse(g.fr.Last("SetAVTransportURI").Args["CurrentURI"])
	if u.Path != "/music/:/transcode/universal/start.mp3" || u.Query().Get("X-Plex-Token") != token || u.Query().Get("offset") != "0" {
		t.Errorf("URI %v", u)
	}
	if !strings.Contains(g.fr.Last("SetAVTransportURI").Args["CurrentURIMetaData"], "audio/mpeg") {
		t.Error("not described as MP3")
	}
}

// A film: the server decides first (token as a header), and the renderer is handed the stream
// described as video, for the device's size: the server's continuous stream from the offset.
func TestPlayAFilm(t *testing.T) {
	g := newRig(t)
	if w := g.playMedia(t, "/library/metadata/3", "&offset=60000"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	dec := g.pms.saw("/video/:/transcode/universal/decision")
	if len(dec) != 1 || strings.Contains(dec[0].URL.RawQuery, token) {
		t.Fatalf("decision requests %d", len(dec))
	}
	set := g.fr.Last("SetAVTransportURI")
	u, _ := url.Parse(set.Args["CurrentURI"])
	q := u.Query()
	if u.Path != "/video/:/transcode/universal/start" || q.Get("offset") != "60" || q.Get("audioStreamID") != "101" ||
		q.Get("session") != dec[0].URL.Query().Get("session") || !strings.Contains(q.Get("X-Plex-Client-Profile-Extra"), "value=720") {
		t.Errorf("start %v", q)
	}
	if !strings.Contains(set.Args["CurrentURIMetaData"], "object.item.videoItem") || !strings.Contains(set.Args["CurrentURIMetaData"], "video/mp2t") {
		t.Errorf("meta %s", set.Args["CurrentURIMetaData"])
	}
	// The renderer's clock starts at the offset's second.
	g.fr.Set(dlna.Playing, "0:00:10")
	g.p.tick(context.Background())
	if s := g.p.snapshot(); s.pos < 70*time.Second || s.pos > 72*time.Second || !s.video {
		t.Errorf("position %v", s.pos)
	}
}

// A seek: the renderer is asked first; one that answers 710 gets a new stream from the offset, once
// for a burst of seeks.
func TestSeek(t *testing.T) {
	g := newRig(t)
	if w := g.playMedia(t, "/library/metadata/3", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	sets := len(g.pms.saw("/video/:/transcode/universal/decision"))
	for _, ms := range []string{"100000", "200000", "300000"} {
		if w := g.get(t, "/player/playback/seekTo?offset="+ms); w.Code != 200 {
			t.Fatalf("seekTo: %d", w.Code)
		}
	}
	time.Sleep(seekWait + 300*time.Millisecond)
	if n := len(g.pms.saw("/video/:/transcode/universal/decision")); n != sets+1 {
		t.Fatalf("%d new decisions for a burst of seeks, want 1", n-sets)
	}
	u, _ := url.Parse(g.fr.Last("SetAVTransportURI").Args["CurrentURI"])
	if u.Query().Get("offset") != "300" {
		t.Errorf("restarted at %q", u.Query().Get("offset"))
	}
	if g.fr.Last("Seek") == nil {
		t.Error("the renderer was not asked to seek first")
	}

	// A renderer that seeks is left to do it: no new stream.
	g.fr.SeekFault = 0
	before := len(g.fr.Calls())
	g.get(t, "/player/playback/seekTo?offset=310000")
	time.Sleep(seekWait + 300*time.Millisecond)
	calls := g.fr.Calls()[before:]
	for _, c := range calls {
		if c.Name == "SetAVTransportURI" {
			t.Error("a seeking renderer was handed a new stream")
		}
	}
	if s := g.fr.Last("Seek"); s.Args["Target"] != "0:00:10" {
		t.Errorf("seek target %q (into the stream that started at 300s)", s.Args["Target"])
	}
}

// A session still handing its new stream to the renderer (a seek that restarts) is not dropped
// because the renderer still plays the stream before it.
func TestARestartingStreamIsNotSomeoneElses(t *testing.T) {
	g := newRig(t)
	if w := g.playMedia(t, "/library/metadata/3", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	s := g.p.current()
	g.p.mu.Lock()
	old := s.stream.uri
	s.stream.uri = ""
	g.p.mu.Unlock()
	g.p.observe(s, dlna.Playing, dlna.Position{URI: old}, time.Now())
	g.p.mu.Lock()
	owned := s.owned
	g.p.mu.Unlock()
	if !owned {
		t.Error("a session mid-restart was dropped as someone else's stream")
	}
}

// A film's timeline names its provider, media and part, and offers the track controls: Plex Web
// builds its audio and subtitle menu from them, and stops the film when they are missing.
func TestAFilmsTimelineOffersTracks(t *testing.T) {
	g := newRig(t)
	if w := g.playMedia(t, "/library/metadata/3", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	body := g.get(t, "/player/timeline/poll?wait=0&commandID=1").Body.String()
	for _, want := range []string{`providerIdentifier="com.plexapp.plugins.library"`, `mediaIndex="0"`, `partIndex="0"`, `audioStream,subtitleStream"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s in %s", want, body)
		}
	}
}

func TestTransport(t *testing.T) {
	g := newRig(t)
	if w := g.get(t, "/player/playback/pause"); w.Code != 500 {
		t.Errorf("pause with nothing: %d", w.Code)
	}
	g.playMedia(t, "/library/metadata/1", "")
	g.fr.Set(dlna.Playing, "0:00:05")
	g.p.tick(context.Background())
	for _, c := range []struct{ cmd, action, state string }{
		{"pause", "Pause", "paused"}, {"play", "Play", "playing"}, {"stop", "Stop", "stopped"},
	} {
		before := len(g.fr.Calls())
		if w := g.get(t, "/player/playback/"+c.cmd); w.Code != 200 {
			t.Fatalf("%s: %d", c.cmd, w.Code)
		}
		found := false
		for _, a := range g.fr.Calls()[before:] {
			found = found || a.Name == c.action
		}
		if !found {
			t.Errorf("%s did not reach the renderer", c.cmd)
		}
		if s := g.p.snapshot(); s.state != c.state {
			t.Errorf("after %s: %q", c.cmd, s.state)
		}
	}
}

// The volume is the device's: setParameters goes to RenderingControl, and the timeline says it.
func TestVolume(t *testing.T) {
	g := newRig(t)
	if w := g.get(t, "/player/playback/setParameters?volume=40"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if a := g.fr.Last("SetVolume"); a == nil || a.Args["DesiredVolume"] != "40" {
		t.Fatalf("SetVolume %+v", a)
	}
	if !strings.Contains(g.timeline(t), `volume="40"`) {
		t.Error("the timeline does not say the volume")
	}
	if w := g.get(t, "/player/playback/setParameters?volume=140"); w.Code != 400 {
		t.Errorf("volume 140: %d", w.Code)
	}
}

// Somebody else taking the renderer stops the item, and nothing is sent to stop theirs.
func TestAnotherCastTakesOver(t *testing.T) {
	g := newRig(t)
	g.playMedia(t, "/library/metadata/1", "")
	g.fr.Set(dlna.Playing, "0:00:05")
	g.p.tick(context.Background())
	r, _ := dlna.Discover(context.Background(), nil, g.fr.Description())
	_ = r.SetURI(context.Background(), "http://192.168.1.30/other.mp3", "")
	g.p.tick(context.Background())
	if s := g.p.snapshot(); s.state != "stopped" {
		t.Errorf("state %q", s.state)
	}
	before := len(g.fr.Calls())
	g.get(t, "/player/playback/pause")
	for _, a := range g.fr.Calls()[before:] {
		if a.Name == "Pause" {
			t.Error("the other cast was paused")
		}
	}
}

// A song played to its end goes on to the next in its queue.
func TestTheQueueGoesOn(t *testing.T) {
	g := newRig(t)
	g.playMedia(t, "/library/metadata/1", "&containerKey="+url.QueryEscape("/playQueues/7?own=1"))
	g.fr.Set(dlna.Playing, "0:02:55")
	g.p.tick(context.Background())
	g.fr.Set(dlna.Stopped, "0:00:00")
	g.p.tick(context.Background())
	if s := g.p.snapshot(); s.it == nil || s.it.ratingKey != "2" {
		t.Fatalf("not on to the next: %+v", s.it)
	}
	if w := g.get(t, "/player/playback/skipPrevious"); w.Code != 200 {
		t.Fatalf("skipPrevious %d", w.Code)
	}
	if s := g.p.snapshot(); s.it.ratingKey != "1" {
		t.Errorf("skipPrevious went to %q", s.it.ratingKey)
	}
}

// setStreams restarts a film with the other track, and tells the server.
func TestSetStreams(t *testing.T) {
	g := newRig(t)
	g.playMedia(t, "/library/metadata/3", "")
	if w := g.get(t, "/player/playback/setStreams?audioStreamID=102&subtitleStreamID=103"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	q, _ := url.Parse(g.fr.Last("SetAVTransportURI").Args["CurrentURI"])
	if q.Query().Get("audioStreamID") != "102" || q.Query().Get("subtitleStreamID") != "103" || q.Query().Get("subtitles") != "burn" {
		t.Errorf("restart %v", q.Query())
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(g.pms.saw("/library/parts/13")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p := g.pms.saw("/library/parts/13"); len(p) == 0 || p[0].Method != http.MethodPut || strings.Contains(p[0].URL.RawQuery, token) {
		t.Error("the server was not told the choice, or was told it with the token in the query")
	}
	if !strings.Contains(g.timeline(t), `subtitleStreamID="103"`) {
		t.Error("the timeline does not say the subtitles")
	}
}

// Subscribers are pushed to, and capped.
func TestSubscribersAreCapped(t *testing.T) {
	g := newRig(t)
	got := make(chan string, 16)
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
	}))
	defer ctrl.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ctrl.URL, "http://"))
	g.p.MaxSubscribers = 2
	sub := func(id string) int {
		r := httptest.NewRequest("GET", "/player/timeline/subscribe?protocol=http&port="+port+"&commandID=1", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:5555", "192.168.1.9:32500"
		r.Header.Set("X-Plex-Client-Identifier", id)
		w := httptest.NewRecorder()
		g.h.ServeHTTP(w, r)
		return w.Code
	}
	if sub("a") != 200 || sub("b") != 200 || sub("a") != 200 {
		t.Fatal("subscribing failed")
	}
	if code := sub("c"); code != http.StatusServiceUnavailable {
		t.Errorf("a third subscriber: %d", code)
	}
	select {
	case body := <-got:
		if !strings.Contains(body, "<MediaContainer") || strings.Contains(body, token) {
			t.Errorf("pushed %s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing was pushed")
	}
}

// A held poll is let go by a change.
func TestAHeldPollIsLetGo(t *testing.T) {
	g := newRig(t)
	done := make(chan int, 1)
	go func() { done <- g.get(t, "/player/timeline/poll?wait=1").Code }()
	time.Sleep(100 * time.Millisecond)
	g.get(t, "/player/playback/setParameters?shuffle=1")
	select {
	case code := <-done:
		if code != 200 {
			t.Errorf("poll %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the poll was held past the change")
	}
}

func TestQueueIDs(t *testing.T) {
	for _, c := range []struct{ key, queue, item string }{
		{"/playQueues/12?window=100&own=1", "12", ""},
		{"/playQueues/7?playQueueItemID=99&own=1", "7", "99"},
		{"", "", ""},
	} {
		if q, i := queueIDs(c.key); q != c.queue || i != c.item {
			t.Errorf("%q: %q %q", c.key, q, i)
		}
	}
}

// Next and Previous pressed on the device's own Now Playing come as the renderer's events and skip
// through the queue, as skipNext and skipPrevious do; only while the renderer has our stream.
func TestSkipFromTheDevice(t *testing.T) {
	g := newRig(t)
	r, err := dlna.Discover(context.Background(), nil, g.fr.Description())
	if err != nil {
		t.Fatal(err)
	}
	ev := &dlna.Events{URL: func() string { return r.AVTEvents }, On: g.p.Event}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = ev.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatal(what)
			}
		}
	}
	waitFor("not subscribed", func() bool { return g.fr.Subscribers() == 1 })
	time.Sleep(100 * time.Millisecond) // the first event, the whole state

	g.playMedia(t, "/library/metadata/1", "&containerKey="+url.QueryEscape("/playQueues/7?own=1"))
	g.fr.Set(dlna.Playing, "0:00:10")
	g.p.tick(context.Background())
	playing := func() string { return g.fr.Last("SetAVTransportURI").Args["CurrentURI"] }
	key := func() string {
		if s := g.p.snapshot(); s.it != nil {
			return s.it.ratingKey
		}
		return ""
	}
	g.fr.Notify([2]string{"TransportState", dlna.Playing}, [2]string{"CurrentTrackURI", playing()}, [2]string{dlna.RequestVar, "next:1"})
	waitFor("next from the device did not skip", func() bool { return key() == "2" })
	g.fr.Notify([2]string{"TransportState", dlna.Playing}, [2]string{"CurrentTrackURI", playing()}, [2]string{dlna.RequestVar, "previous:2"})
	waitFor("previous from the device did not skip back", func() bool { return key() == "1" })

	// The player said it acts on seeks when it subscribed, and a place chosen on the device's bar
	// moves the renderer there, as seekTo does.
	if got := g.fr.Requests(); got != "next, previous, seek" {
		t.Errorf("subscribed saying it acts on %q", got)
	}
	g.fr.Notify([2]string{"TransportState", dlna.Playing}, [2]string{"CurrentTrackURI", playing()}, [2]string{dlna.RequestVar, "seek:95:3"})
	waitFor("a seek from the device did not move the renderer", func() bool {
		c := g.fr.Last("Seek")
		return c != nil && c.Args["Target"] == "0:01:35"
	})

	// Another cast has the renderer: its Next is not ours to act on.
	_ = r.SetURI(context.Background(), "http://192.168.1.30/other.mp3", "")
	g.fr.Set(dlna.Playing, "0:00:01")
	g.p.tick(context.Background())
	before := len(g.fr.Calls())
	g.fr.Notify([2]string{"TransportState", dlna.Playing}, [2]string{"CurrentTrackURI", "http://192.168.1.30/other.mp3"}, [2]string{dlna.RequestVar, "next:4"})
	time.Sleep(200 * time.Millisecond)
	for _, a := range g.fr.Calls()[before:] {
		if a.Name == "SetAVTransportURI" {
			t.Fatal("another cast's Next skipped our queue")
		}
	}
	if key() != "1" {
		t.Errorf("on %q", key())
	}
}

// A state change in an event has the renderer read at once.
func TestAnEventNudges(t *testing.T) {
	g := newRig(t)
	g.p.Event(dlna.Event{Vars: map[string]string{"TransportState": dlna.Paused}})
	select {
	case <-g.p.nudge:
	default:
		t.Error("no nudge")
	}
	g.p.Event(dlna.Event{Vars: map[string]string{"Volume": "3"}})
	select {
	case <-g.p.nudge:
		t.Error("nudged with no state")
	default:
	}
}

// A film is played only from a server the linked account lists, at an address it lists for it: the
// device's renderer plays a film from this program without asking on the screen, and this program
// relays what any device on the network sends. A song from such a server still plays.
func TestAFilmFromAServerTheAccountDoesNotListIsRefused(t *testing.T) {
	g := newRig(t)
	g.p.ServerToken = func(context.Context, string, string) string { return "" }
	if w := g.playMedia(t, "/library/metadata/3", ""); w.Code == 200 {
		t.Fatal("a film from a server the account does not list was played")
	}
	if g.fr.Last("SetAVTransportURI") != nil {
		t.Error("the renderer was handed it")
	}
	if w := g.playMedia(t, "/library/metadata/1", ""); w.Code != 200 {
		t.Errorf("a song: %d", w.Code)
	}

	// No account linked at all: the same.
	g = newRig(t)
	g.p.ServerToken = nil
	if w := g.playMedia(t, "/library/metadata/3", ""); w.Code == 200 {
		t.Fatal("a film was played with no account linked")
	}
}
