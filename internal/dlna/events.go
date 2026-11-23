package dlna

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RequestVar is the vendor state variable TECHO5's renderer sets in an AVTransport event when Next or
// Previous is pressed on the device's own Now Playing for a DLNA item: "next" or "previous", maybe
// with a count after a colon ("next:3") so that one press is told from the next.
const RequestVar = "X_TECHO5_Request"

// Event is one AVTransport event.
type Event struct {
	// Vars are LastChange's state variables for instance 0, by name. CurrentTrackURI can carry a
	// token: compare it, never log it.
	Vars map[string]string
	// Request is the word in RequestVar ("next", "previous", "seek"), set only when it is new: not in
	// the first event of a subscription (the whole state, sent at once) and not a repeat of the last one.
	Request string
	// At is a seek request's place ("seek:95:7" is 95 seconds), 0 for any other request.
	At time.Duration
}

// TransportState is the event's TransportState, "" when it has none.
func (e Event) TransportState() string { return e.Vars["TransportState"] }

// Events holds a subscription to a renderer's AVTransport events (UPnP GENA) open and hands each event
// over. Its own listener is on loopback: the renderer is the device's own, and calls back only the
// subscriber's address.
type Events struct {
	// URL is the event subscription URL, read at every turn; "" while the renderer is not found.
	URL func() string
	// On is given each event, in order. It should not block.
	On func(Event)
	// HTTP is for SUBSCRIBE and UNSUBSCRIBE (a 10-second client when nil).
	HTTP *http.Client
	// Listen is the callback's address ("127.0.0.1:0" when empty).
	Listen string
	// Timeout is the subscription asked for (30 minutes when 0).
	Timeout time.Duration
	// RenewEvery caps the time between renewals (a minute when 0). A renderer that restarted has
	// forgotten the subscription and refuses the renewal, which is how a restart is noticed.
	RenewEvery time.Duration
	// Retry is the pause after a failed subscription (10 seconds when 0).
	Retry time.Duration

	mu       sync.Mutex
	sid      string
	url      string
	callback string
	primed   bool          // the subscription's first event has been seen
	seq      uint64        // the last SEQ taken
	lastReq  string        // RequestVar's value in the last event taken
	pending  chan struct{} // closed when a new subscription's answer has been read
}

const mostEvent = 256 << 10

// Run subscribes and renews until ctx ends, then unsubscribes.
func (e *Events) Run(ctx context.Context) error {
	listen := e.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: http.HandlerFunc(e.notify), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	e.mu.Lock()
	e.callback = "http://" + ln.Addr().String() + "/event"
	e.mu.Unlock()

	retry := or(e.Retry, 10*time.Second)
	for {
		wait := retry
		if u := e.URL(); u != "" {
			if granted, err := e.keep(ctx, u); err == nil {
				wait = min(granted/2, or(e.RenewEvery, time.Minute))
			} else if ctx.Err() == nil {
				slog.Debug("plex: the renderer's events could not be subscribed to", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			e.unsubscribe()
			return nil
		case <-time.After(wait):
		}
	}
}

// keep renews the subscription to u, or makes one when there is none, u changed, or the renewal was
// refused.
func (e *Events) keep(ctx context.Context, u string) (time.Duration, error) {
	e.mu.Lock()
	sid, cur := e.sid, e.url
	e.mu.Unlock()
	if sid != "" && cur == u {
		granted, _, err := e.subscribe(ctx, u, sid)
		if err == nil {
			return granted, nil
		}
		slog.Debug("plex: the renderer refused the renewal, subscribing again", "err", err)
	}
	if sid != "" {
		e.unsubscribe()
	}
	granted, _, err := e.subscribe(ctx, u, "")
	if err != nil {
		return 0, err
	}
	slog.Debug("plex: subscribed to the renderer's events")
	return granted, nil
}

// subscribe sends SUBSCRIBE: a new subscription when sid is "", else its renewal.
func (e *Events) subscribe(ctx context.Context, u, sid string) (time.Duration, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "SUBSCRIBE", u, nil)
	if err != nil {
		return 0, "", err
	}
	timeout := or(e.Timeout, 30*time.Minute)
	req.Header.Set("TIMEOUT", "Second-"+strconv.Itoa(int(timeout/time.Second)))
	e.mu.Lock()
	callback := e.callback
	e.mu.Unlock()
	if sid != "" {
		req.Header.Set("SID", sid)
	} else {
		req.Header.Set("CALLBACK", "<"+callback+">")
		req.Header.Set("NT", "upnp:event")
		// Which of TECHO5's own requests this player acts on: the device passes a place chosen on its
		// screen on only to a controller that says it acts on seeks.
		req.Header.Set("X-TECHO5-Requests", "next, previous, seek")
		// A new subscription's first event can come as soon as the answer is out, before the SID is
		// recorded: notify waits for this to settle.
		settled := make(chan struct{})
		e.mu.Lock()
		e.sid, e.pending = "", settled
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			if e.pending == settled {
				e.pending = nil
			}
			e.mu.Unlock()
			close(settled)
		}()
	}
	resp, err := e.client().Do(req)
	if err != nil {
		return 0, "", err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if sid != "" {
			e.mu.Lock()
			if e.sid == sid {
				e.sid = ""
			}
			e.mu.Unlock()
		}
		return 0, "", fmt.Errorf("the renderer answered %s", resp.Status)
	}
	got := strings.TrimSpace(resp.Header.Get("SID"))
	if got == "" {
		got = sid
	}
	if got == "" {
		return 0, "", errors.New("the renderer gave no SID")
	}
	granted := parseTimeout(resp.Header.Get("TIMEOUT"), timeout)
	e.mu.Lock()
	if sid == "" || got != sid {
		e.sid, e.url, e.primed, e.seq, e.lastReq = got, u, false, 0, ""
	}
	e.mu.Unlock()
	return granted, got, nil
}

// unsubscribe drops the subscription, best effort: the renderer keeps only so many per subscriber.
func (e *Events) unsubscribe() {
	e.mu.Lock()
	sid, u := e.sid, e.url
	e.sid = ""
	e.mu.Unlock()
	if sid == "" || u == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "UNSUBSCRIBE", u, nil)
	if err != nil {
		return
	}
	req.Header.Set("SID", sid)
	if resp, err := e.client().Do(req); err == nil {
		resp.Body.Close()
	}
}

func (e *Events) client() *http.Client {
	if e.HTTP != nil {
		return e.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// notify takes one NOTIFY.
func (e *Events) notify(w http.ResponseWriter, r *http.Request) {
	if r.Method != "NOTIFY" || r.URL.Path != "/event" {
		http.Error(w, "no", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("NT") != "upnp:event" || r.Header.Get("NTS") != "upnp:propchange" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, mostEvent))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	vars, err := ParseLastChange(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	seq, seqErr := strconv.ParseUint(strings.TrimSpace(r.Header.Get("SEQ")), 10, 32)
	e.mu.Lock()
	if wait := e.pending; wait != nil && r.Header.Get("SID") != e.sid {
		e.mu.Unlock()
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
		}
		e.mu.Lock()
	}
	if e.sid == "" || r.Header.Get("SID") != e.sid {
		e.mu.Unlock()
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	ev := Event{Vars: vars}
	req := vars[RequestVar]
	switch {
	case !e.primed || seq == 0:
		// The whole state, sent on subscribing: whatever request it holds was made before.
		e.primed, e.seq, e.lastReq = true, seq, req
	case seqErr == nil && seq <= e.seq:
		// Overtaken by a later event: the renderer sends each in its own connection.
		e.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	default:
		if seqErr == nil {
			e.seq = seq
		}
		if req != "" && req != e.lastReq {
			ev.Request = RequestWord(req)
			if ev.Request == "seek" {
				ev.At = RequestPlace(req)
			}
		}
		e.lastReq = req
	}
	if e.On != nil {
		e.On(ev)
	}
	e.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// RequestWord is a request value's word: what comes before an optional ":", in lower case.
func RequestWord(v string) string {
	w, _, _ := strings.Cut(v, ":")
	return strings.ToLower(strings.TrimSpace(w))
}

// RequestPlace is a seek request's place: the whole seconds after "seek:" and before the request's
// number ("seek:95:7" is 95 seconds), 0 when there is none.
func RequestPlace(v string) time.Duration {
	parts := strings.Split(v, ":")
	if len(parts) < 3 {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// ParseLastChange reads an event's propertyset: the LastChange property (XML escaped inside XML) and
// its variables for instance 0, each by name with its val attribute.
func ParseLastChange(body []byte) (map[string]string, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var inner strings.Builder
	in, found := false, false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "LastChange" {
				in, found = true, true
			}
		case xml.CharData:
			if in {
				inner.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "LastChange" {
				in = false
			}
		}
	}
	vars := map[string]string{}
	if !found {
		return vars, nil
	}
	d = xml.NewDecoder(strings.NewReader(inner.String()))
	depth, instDepth := 0, 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return vars, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "InstanceID" {
				if attr(t, "val") == "0" {
					instDepth = depth
				}
				continue
			}
			if instDepth != 0 && depth == instDepth+1 {
				vars[t.Name.Local] = attr(t, "val")
			}
		case xml.EndElement:
			if depth == instDepth {
				instDepth = 0
			}
			depth--
		}
	}
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// parseTimeout reads "Second-1800"; anything else ("infinite" included) is def.
func parseTimeout(h string, def time.Duration) time.Duration {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(h), "Second-"))
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
