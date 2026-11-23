// Package companion is the Plex Companion player: the HTTP server on port 32500 that casting apps
// send playMedia, play, pause, seekTo and the rest to, and the timelines they are told what the player
// is doing with. The playing itself is the device's own DLNA renderer's: this package resolves what
// to fetch from the Plex server and hands it over.
//
// Adapted from kevin909867's plexcast (TECHO5 PR #691): server.go, playmedia.go, playback.go,
// timeline.go, queue.go, report.go, streams.go and transcode.go.
package companion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/dlna"
	"github.com/HuskerMinion/techo5-plex/internal/netguard"
	"github.com/HuskerMinion/techo5-plex/internal/plextv"
)

const (
	// Port is the Companion control port.
	Port = 32500

	// ProtocolVersion is advertised as 3, as Plex Web advertises it, which controllers read as "at
	// least the version I need".
	ProtocolVersion = 3

	// Capabilities is what a controller may ask of the player.
	Capabilities = "timeline,playback,playqueues"
)

// Renderer is what the player drives: the device's DLNA renderer (dlna.Renderer).
type Renderer interface {
	SetURI(ctx context.Context, uri, meta string) error
	Play(ctx context.Context) error
	Pause(ctx context.Context) error
	Stop(ctx context.Context) error
	Seek(ctx context.Context, pos time.Duration) error
	TransportState(ctx context.Context) (string, error)
	Position(ctx context.Context) (dlna.Position, error)
	SetVolume(ctx context.Context, v int) error
	Volume(ctx context.Context) (int, error)
}

// Player is the device as a Plex player.
type Player struct {
	polls atomic.Int64 // timeline polls, for the log
	ID    *plextv.Identity
	// Renderer is the renderer once it has been found, nil before.
	Renderer func() Renderer
	// Fetch is for the Plex server a controller named (netguard.Client: never the device itself).
	Fetch *http.Client
	// Push is for timelines pushed to subscribed controllers (also netguard.Client).
	Push *http.Client
	// VideoW and VideoH are the largest picture the server is asked for.
	VideoW, VideoH int
	// MaxSubscribers caps the controllers pushed to.
	MaxSubscribers int
	// RefuseAddr is whether a server address a controller names is refused before anything is fetched
	// (netguard.Refused: the device itself, loopback, link-local). Fetch's dial check backs it up.
	RefuseAddr func(net.IP) bool
	// ServerToken, when set, is the linked account's own token for the server machine at host, or ""
	// when the account does not list host for it. A cast's token may not be allowed to change a part.
	ServerToken func(ctx context.Context, machine, host string) string
	// OnServer, when set, is told the address of each server a cast comes from (so GDM can announce
	// to it by unicast).
	OnServer func(net.IP)

	mu    sync.Mutex
	cur   *session
	gen   int
	vol   int
	volAt time.Time

	tl    *timeline
	nudge chan struct{} // a tick now, from the renderer's events

	trustMu sync.Mutex
	trust   map[string]trusted // accountServer's answers, by server and address
}

// New is a player with the add-on's defaults.
func New(id *plextv.Identity, renderer func() Renderer) *Player {
	p := &Player{
		ID:             id,
		Renderer:       renderer,
		Fetch:          netguard.Client(0),
		Push:           netguard.Client(5 * time.Second),
		VideoW:         1280,
		VideoH:         720,
		MaxSubscribers: 8,
		RefuseAddr:     netguard.Refused,
		vol:            -1,
		nudge:          make(chan struct{}, 1),
	}
	p.tl = newTimeline(p)
	return p
}

// Handler is everything the Companion server answers.
func (p *Player) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/resources", p.resources)
	m.HandleFunc("/player/timeline/poll", p.poll)
	m.HandleFunc("/player/timeline/subscribe", p.subscribe)
	m.HandleFunc("/player/timeline/unsubscribe", p.unsubscribe)
	m.HandleFunc("/player/playback/playMedia", p.playMedia)
	m.HandleFunc("/player/playback/play", p.command(func(ctx context.Context, _ query) error { return p.play(ctx) }))
	m.HandleFunc("/player/playback/pause", p.command(func(ctx context.Context, _ query) error { return p.pause(ctx) }))
	m.HandleFunc("/player/playback/stop", p.command(func(ctx context.Context, _ query) error { return p.stop(ctx) }))
	m.HandleFunc("/player/playback/seekTo", p.command(func(ctx context.Context, q query) error {
		return p.seek(ctx, q.millis("offset"))
	}))
	m.HandleFunc("/player/playback/stepForward", p.command(func(ctx context.Context, _ query) error { return p.step(ctx, 30*time.Second) }))
	m.HandleFunc("/player/playback/stepBack", p.command(func(ctx context.Context, _ query) error { return p.step(ctx, -15*time.Second) }))
	m.HandleFunc("/player/playback/skipNext", p.command(func(ctx context.Context, _ query) error { return p.skip(ctx, 1) }))
	m.HandleFunc("/player/playback/skipPrevious", p.command(func(ctx context.Context, _ query) error { return p.skip(ctx, -1) }))
	m.HandleFunc("/player/playback/setParameters", p.setParameters)
	m.HandleFunc("/player/playback/setStreams", p.command(func(ctx context.Context, q query) error {
		return p.setStreams(ctx, q.number("audioStreamID", -1), q.number("subtitleStreamID", -1))
	}))
	m.HandleFunc("/player/playback/refreshPlayQueue", p.refreshPlayQueue)
	// Navigation and mirroring are not offered; answered rather than left to a 404.
	for _, path := range []string{"/player/navigation/", "/player/mirror/", "/player/application/"} {
		m.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) { agree(w) })
	}
	return p.wrap(m)
}

// wrap is what every request needs:
//
//   - the home network only, and an address in Host (a name there is DNS rebinding from a browser);
//   - X-Plex-Target-Client-Identifier, when sent, must be this player (404 otherwise, as the spec says);
//   - the CORS headers Plex Web needs, with X-Plex-Client-Identifier exposed, or it ignores every
//     timeline;
//   - the controller's commandID recorded for its subscription.
func (p *Player) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/player/timeline/poll" || p.polls.Add(1)%30 == 1 {
			slog.Debug("plex: a request", "path", r.URL.Path, "from", netguard.RemoteIP(r).String(),
				"params", paramNames(r), "polls", p.polls.Load())
		}
		if !netguard.Home(netguard.RemoteIP(r)) || !netguard.HostIsAddress(r.Host) {
			slog.Info("plex: a request refused", "path", r.URL.Path, "from", netguard.RemoteIP(r).String(), "host", r.Host)
			http.Error(w, "not from here", http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "X-Plex-Client-Identifier,X-Plex-Target-Client-Identifier,X-Plex-Token,X-Plex-Device-Name,X-Plex-Platform,X-Plex-Product,X-Plex-Version,Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "X-Plex-Client-Identifier")
		w.Header().Set("X-Plex-Client-Identifier", p.ID.ClientID())
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if t := r.Header.Get("X-Plex-Target-Client-Identifier"); t != "" && t != p.ID.ClientID() {
			http.Error(w, "not this player", http.StatusNotFound)
			return
		}
		if c := r.URL.Query().Get("commandID"); c != "" {
			p.tl.command(r.Header.Get("X-Plex-Client-Identifier"), c)
		}
		if r.URL.Path == "/player/timeline/poll" {
			// For finding out why a controller gives up on polls: anything but a quick 200 is logged.
			sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
			began := time.Now()
			h.ServeHTTP(sw, r)
			if took := time.Since(began); sw.code != http.StatusOK || took > 5*time.Second || r.Context().Err() != nil {
				slog.Debug("plex: a poll", "status", sw.code, "took", took.Round(time.Millisecond),
					"gone", r.Context().Err() != nil, "params", paramNames(r))
			}
			return
		}
		h.ServeHTTP(w, r)
	})
}

// statusWriter remembers the status a handler answered.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func agree(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// fail is a command that could not be carried out: Plex's Response element.
func fail(w http.ResponseWriter, code int, said string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(code)
	fmt.Fprintf(w, `<Response code="%d" status="%s"/>`, code, dlna.Escape(said))
}

type query struct{ r *http.Request }

func (q query) get(name string) string { return q.r.URL.Query().Get(name) }

func (q query) millis(name string) time.Duration {
	ms, err := strconv.ParseInt(q.get(name), 10, 64)
	if err != nil || ms < 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func (q query) number(name string, def int) int {
	n, err := strconv.Atoi(q.get(name))
	if err != nil {
		return def
	}
	return n
}

func readable(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "commands are GETs")
		return false
	}
	return true
}

// command is a handler for a command that either works (200) or says why not (500).
func (p *Player) command(fn func(context.Context, query) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !readable(w, r) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := fn(ctx, query{r}); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		agree(w)
	}
}

// resources answers what this player is: the liveness probe controllers use.
func (p *Player) resources(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, `<MediaContainer size="1">`+
		`<Player title="`+dlna.Escape(p.ID.Name())+`" machineIdentifier="`+dlna.Escape(p.ID.ClientID())+`"`+
		` product="`+plextv.Product+`" version="`+dlna.Escape(p.ID.Version())+`" platform="`+plextv.Platform+`"`+
		` platformVersion="`+dlna.Escape(p.ID.Version())+`" deviceClass="`+plextv.DeviceClass+`" protocol="plex"`+
		` protocolVersion="`+strconv.Itoa(ProtocolVersion)+`" protocolCapabilities="`+Capabilities+`"/>`+
		`</MediaContainer>`)
}

// serverHeaders are on every request to a Plex server: the identity, and the token as a header.
func (p *Player) serverHeaders(r *http.Request, token string) {
	p.ID.Apply(r)
	if token != "" {
		r.Header.Set("X-Plex-Token", token)
	}
}

// scrub is a transport error without its URL, which may have a query worth keeping out of the log.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// renderer is the renderer, or an error saying it has not been found.
func (p *Player) renderer() (Renderer, error) {
	if p.Renderer != nil {
		if r := p.Renderer(); r != nil {
			return r, nil
		}
	}
	return nil, errNoRenderer
}

var errNoRenderer = errors.New("the device's DLNA renderer was not found: is DLNA on?")

// Serve runs the Companion server on addr until ctx ends.
func (p *Player) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// No ReadTimeout: a timeline poll holds its request on purpose.
	srv := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	slog.Info("plex: Companion server on", "addr", addr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// paramNames is a request's query parameters for the log: their names and short values, never the
// token.
func paramNames(r *http.Request) string {
	var b strings.Builder
	for k, v := range r.URL.Query() {
		if strings.EqualFold(k, "token") || strings.EqualFold(k, "X-Plex-Token") {
			continue
		}
		val := strings.Join(v, ",")
		if len(val) > 60 {
			val = val[:60] + "…"
		}
		b.WriteString(k + "=" + val + " ")
	}
	return b.String()
}
