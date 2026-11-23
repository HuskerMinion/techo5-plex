// Package linkpage is the add-on's own small web page (port 32501, home network only): whether the
// Plex account is linked, the code to type at plex.tv/link while a link is being made, whether the
// device's DLNA renderer was found, and Link, Cancel and Unlink buttons.
package linkpage

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/netguard"
	"github.com/HuskerMinion/techo5-plex/internal/plextv"
)

// Port is the page's port.
const Port = 32501

// Linker is what the buttons do (plextv.Linker).
type Linker interface {
	Status() plextv.Status
	Start() (string, error)
	Cancel()
	Unlink() error
}

// Page is the page.
type Page struct {
	L Linker
	// Name is the player's name.
	Name func() string
	// Renderer is "" when the device's renderer was found, else why not.
	Renderer func() string
	// Playing is what is playing and its state, "" for nothing.
	Playing func() (title, state string)

	form string // the token every button's form carries
}

// New is a page with a fresh form token.
func New(l Linker, name func() string, renderer func() string, playing func() (string, string)) *Page {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return &Page{L: l, Name: name, Renderer: renderer, Playing: playing, form: hex.EncodeToString(b[:])}
}

// Handler serves the page.
func (pg *Page) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", pg.index)
	m.HandleFunc("GET /status.json", pg.status)
	m.HandleFunc("POST /link", pg.action(func() error { _, err := pg.L.Start(); return err }))
	m.HandleFunc("POST /cancel", pg.action(func() error { pg.L.Cancel(); return nil }))
	m.HandleFunc("POST /unlink", pg.action(pg.L.Unlink))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !netguard.Home(netguard.RemoteIP(r)) || !netguard.HostIsAddress(r.Host) {
			http.Error(w, "open this page at the device's address, from the home network", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
		m.ServeHTTP(w, r)
	})
}

// sameOrigin is a POST from this page: the form token matches, and Origin, when the browser sends one,
// is this host.
func (pg *Page) sameOrigin(r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("form")), []byte(pg.form)) != 1 {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

func (pg *Page) action(fn func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if !pg.sameOrigin(r) {
			http.Error(w, "reload the page and try again", http.StatusForbidden)
			return
		}
		if err := fn(); err != nil {
			slog.Warn("plex: a button on the page failed", "err", err)
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

type view struct {
	Name, Renderer, Title, State, Form, LinkURL string
	S                                           plextv.Status
}

func (pg *Page) view() view {
	v := view{Name: pg.Name(), S: pg.L.Status(), Form: pg.form, LinkURL: plextv.LinkURL}
	if pg.Renderer != nil {
		v.Renderer = pg.Renderer()
	}
	if pg.Playing != nil {
		v.Title, v.State = pg.Playing()
	}
	return v
}

func (pg *Page) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, pg.view()); err != nil {
		slog.Debug("plex: the page was not written", "err", err)
	}
}

func (pg *Page) status(w http.ResponseWriter, r *http.Request) {
	v := pg.view()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		plextv.Status
		Name     string `json:"name"`
		Renderer string `json:"renderer,omitempty"`
		Playing  string `json:"playing,omitempty"`
		State    string `json:"state,omitempty"`
	}{v.S, v.Name, v.Renderer, v.Title, v.State})
}

var tmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .S.Waiting}}<meta http-equiv="refresh" content="3">{{end}}
<title>Plex on {{.Name}}</title>
<style>
:root{color-scheme:light dark;--bg:#f6f6f4;--fg:#1d1d1b;--card:#fff;--muted:#6b6b66;--accent:#c8810b;--warn:#a23b2a}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--fg:#ececea;--card:#22221f;--muted:#a3a39c;--accent:#e5a00d;--warn:#e07a65}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{max-width:34rem;margin:0 auto;padding:24px 16px}
h1{font-size:1.4rem;margin:0 0 16px}
.card{background:var(--card);border-radius:12px;padding:16px 20px;margin-bottom:16px}
.code{font:700 2.6rem/1.2 ui-monospace,monospace;letter-spacing:.3em;color:var(--accent)}
.muted{color:var(--muted)}.warn{color:var(--warn)}
button{font:inherit;padding:8px 18px;border-radius:8px;border:1px solid var(--muted);background:transparent;color:var(--fg);cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:#1d1d1b}
form{display:inline}
</style></head><body><main>
<h1>Plex on {{.Name}}</h1>
{{if .Renderer}}<div class="card warn">{{.Renderer}}</div>{{end}}
<div class="card">
{{if .S.Linked}}
<p>Linked to Plex{{if .S.User}} as <strong>{{.S.User}}</strong>{{end}}.</p>
<form method="post" action="/unlink"><input type="hidden" name="form" value="{{.Form}}"><button>Unlink</button></form>
{{else if .S.Waiting}}
{{if .S.Code}}<p>Go to <strong>{{.LinkURL}}</strong> and enter:</p><p class="code">{{.S.Code}}</p>
<p class="muted">The code is good for a few minutes. This page updates by itself.</p>
{{else}}<p class="muted">Asking plex.tv for a code…</p>{{end}}
<form method="post" action="/cancel"><input type="hidden" name="form" value="{{.Form}}"><button>Cancel</button></form>
{{else}}
<p>Not linked. Casting from Plex apps on this network can work without an account; linking lets
the Plex apps that only list your account's players find it too.</p>
{{if .S.Err}}<p class="warn">{{.S.Err}}</p>{{end}}
<form method="post" action="/link"><input type="hidden" name="form" value="{{.Form}}"><button class="primary">Link</button></form>
{{end}}
</div>
<div class="card"><p class="muted">{{if .Title}}{{.State}}: {{.Title}}{{else}}Nothing playing.{{end}}</p></div>
</main></body></html>
`))

// Serve runs the page on addr until ctx ends.
func (pg *Page) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: pg.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
