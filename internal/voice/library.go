package voice

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/plextv"
)

// The library: every film and show in the video sections of the account's servers on the home
// network, by title. Kept in memory only, never written down: read again on link, every hour, and when
// a title is not found in a list older than a couple of minutes.

// Entry is a film or a show.
type Entry struct {
	Kind      string // KindMovie or KindShow
	Title     string
	Year      int
	RatingKey string
	server    *server
	norm      string // titleKey(Title)
}

// server is one of the account's servers as the library reaches it.
type server struct {
	plextv.Server
	base string // the address that answered
}

// mostEntries bounds the library, and mostListing what one section's listing may come to.
const (
	mostEntries = 50000
	mostListing = 64 << 20
)

// library is the list, and when it was read.
type library struct {
	mu      sync.Mutex
	entries []*Entry
	servers []*server
	at      time.Time
	token   string // the account token it was read with
	err     error  // why the last reading failed, if it did
}

func (l *library) snapshot() ([]*Entry, time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries, l.at, l.err
}

// read lists the library of every server, and keeps it in place of the old one. A server that does
// not answer is left out; none answering is an error, and the old list stays.
func (v *Voice) readLibrary(ctx context.Context, token string) error {
	list, err := v.Servers(ctx, token)
	if err != nil {
		v.lib.mu.Lock()
		v.lib.err = err
		v.lib.mu.Unlock()
		return err
	}
	var (
		entries []*Entry
		servers []*server
		seen    = map[string]bool{} // kind, title and year: the same film on two servers is one
		lastErr error
	)
	for _, s := range list {
		secret(s.Token)
		srv, got, err := v.readServer(ctx, s)
		if err != nil {
			lastErr = err
			slog.Debug("plex: a server's library could not be read", "server", s.Name, "err", err)
			continue
		}
		servers = append(servers, srv)
		for _, e := range got {
			k := e.Kind + "|" + e.norm + "|" + strconv.Itoa(e.Year)
			if seen[k] || len(entries) >= mostEntries {
				continue
			}
			seen[k] = true
			entries = append(entries, e)
		}
	}
	if len(servers) == 0 {
		if lastErr == nil {
			lastErr = errNoServer
		}
		v.lib.mu.Lock()
		v.lib.err = lastErr
		v.lib.mu.Unlock()
		return lastErr
	}
	v.lib.mu.Lock()
	v.lib.entries, v.lib.servers, v.lib.at, v.lib.token, v.lib.err = entries, servers, time.Now(), token, nil
	v.lib.mu.Unlock()
	slog.Debug("plex: library read", "servers", len(servers), "titles", len(entries))
	return nil
}

var errNoServer = errors.New("the account lists no Plex server on the home network")

// readServer lists one server's films and shows, at the first of its addresses that answers.
func (v *Voice) readServer(ctx context.Context, s plextv.Server) (*server, []*Entry, error) {
	var lastErr error
	for _, base := range s.Bases {
		srv := &server{Server: s, base: base}
		var sections struct {
			Directories []struct {
				Key  string `xml:"key,attr"`
				Type string `xml:"type,attr"`
			} `xml:"Directory"`
		}
		if err := v.fetch(ctx, srv, "/library/sections", func(r io.Reader) error {
			return xml.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&sections)
		}); err != nil {
			lastErr = err
			continue
		}
		var out []*Entry
		for _, d := range sections.Directories {
			typ := ""
			switch d.Type {
			case "movie":
				typ = "1"
			case "show":
				typ = "2"
			default:
				continue
			}
			if _, err := strconv.Atoi(d.Key); err != nil {
				continue
			}
			path := "/library/sections/" + d.Key + "/all?type=" + typ + "&includeCollections=0"
			if err := v.fetch(ctx, srv, path, func(r io.Reader) error {
				return listing(io.LimitReader(r, mostListing), srv, &out)
			}); err != nil {
				return nil, nil, err
			}
		}
		return srv, out, nil
	}
	return nil, nil, lastErr
}

// listing reads a section's listing an element at a time, keeping only what matching needs: a film's
// or a show's listing can run to megabytes.
func listing(r io.Reader, srv *server, out *[]*Entry) error {
	d := xml.NewDecoder(r)
	for len(*out) < mostEntries {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("the listing was not readable: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || (se.Name.Local != "Video" && se.Name.Local != "Directory") {
			continue
		}
		e := &Entry{server: srv}
		var typ string
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "type":
				typ = a.Value
			case "title":
				e.Title = a.Value
			case "year":
				e.Year, _ = strconv.Atoi(a.Value)
			case "ratingKey":
				e.RatingKey = a.Value
			}
		}
		switch typ {
		case "movie":
			e.Kind = KindMovie
		case "show":
			e.Kind = KindShow
		default:
			continue
		}
		// A rating key goes into paths on the server: Plex's are numbers, and nothing else is taken.
		if e.Title == "" || e.RatingKey == "" || strings.Trim(e.RatingKey, "0123456789") != "" {
			continue
		}
		e.norm = titleKey(e.Title)
		*out = append(*out, e)
		if err := d.Skip(); err != nil {
			return fmt.Errorf("the listing was not readable: %w", err)
		}
	}
	return nil
}

// fetch GETs path on the server with the account's token on it as a header, and hands the answer to
// read. Errors say the path and the status, never an address with a token.
func (v *Voice) fetch(ctx context.Context, srv *server, path string, read func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.base+path, nil)
	if err != nil {
		return errors.New("the server's address is not usable")
	}
	v.Player.ID.Apply(req)
	req.Header.Set("X-Plex-Token", srv.Token)
	req.Header.Set("Accept", "application/xml")
	resp, err := v.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s: %w", ue.Op, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("the server answered %d for %s", resp.StatusCode, pathOnly(path))
	}
	return read(resp.Body)
}

func pathOnly(p string) string {
	if u, err := url.Parse(p); err == nil {
		return u.Path
	}
	return "a request"
}

// transientFor is how long a transient token is used before another is asked for. The server keeps
// one for 48 hours, or until it restarts.
const transientFor = time.Hour

type transient struct {
	token string
	at    time.Time
}

// source is the server as the player uses it, with a transient token: the one that goes in the
// address the device's player fetches, so the account's own token never leaves the add-on.
func (v *Voice) source(ctx context.Context, srv *server) (Source, error) {
	v.tmu.Lock()
	t, ok := v.transients[srv.Machine]
	v.tmu.Unlock()
	if ok && time.Since(t.at) < transientFor {
		return Source{Base: srv.base, Machine: srv.Machine, Token: t.token}, nil
	}
	var c struct {
		Token string `xml:"token,attr"`
	}
	if err := v.fetch(ctx, srv, "/security/token?type=delegation&scope=all", func(r io.Reader) error {
		return xml.NewDecoder(io.LimitReader(r, 64<<10)).Decode(&c)
	}); err != nil {
		return Source{}, fmt.Errorf("the server gave no token to play with: %w", err)
	}
	if c.Token == "" {
		return Source{}, errors.New("the server gave no token to play with")
	}
	secret(c.Token)
	v.tmu.Lock()
	if v.transients == nil {
		v.transients = map[string]transient{}
	}
	v.transients[srv.Machine] = transient{c.Token, time.Now()}
	v.tmu.Unlock()
	return Source{Base: srv.base, Machine: srv.Machine, Token: c.Token}, nil
}

// dropTransient forgets the server's transient token, for a new one at the next command.
func (v *Voice) dropTransient(srv *server) {
	v.tmu.Lock()
	delete(v.transients, srv.Machine)
	v.tmu.Unlock()
}

// Known says whether what was said is exactly a film or show in the library as it is kept now: the
// same title once normalized ("fifty first dates" is 50 First Dates), with or without its year, and
// with "from the beginning" or a season and episode after it. It never reads the library while the
// caller waits: one not read yet for this account is read in the background, and is not known yet.
// Home Assistant asks it before a "play ..." goes to music, so it has to be quick and sure.
func (v *Voice) Known(said string) bool {
	token := v.Token()
	if token == "" {
		return false
	}
	v.lib.mu.Lock()
	entries, read := v.lib.entries, v.lib.token == token && !v.lib.at.IsZero()
	v.lib.mu.Unlock()
	if !read {
		v.readSoon(token)
		return false
	}
	title, _ := fromStart(said, nil)
	if m := spokenEpisode.FindStringSubmatch(title); m != nil {
		title = m[1]
	}
	q := parseQuery(title)
	if q.full == "" {
		return false
	}
	for _, e := range entries {
		if e.norm == q.full || (q.year != 0 && e.norm == q.bare && e.Year == q.year) {
			return true
		}
	}
	return false
}

// readSoon reads the library in the background, once at a time.
func (v *Voice) readSoon(token string) {
	if !v.reading.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer v.reading.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), commandFor)
		defer cancel()
		if err := v.readLibrary(ctx, token); err != nil {
			slog.Debug("plex: the library was not read", "err", err)
		}
	}()
}
