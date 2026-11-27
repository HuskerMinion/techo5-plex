package companion

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/plextv"
)

// The calls to the item's Plex server besides the stream: the play queue (skips), the stream choice,
// and the reports that keep the server's Now Playing and resume point in step. The token goes as a
// header on every one.

// next is the item delta places from the one playing in its queue.
func (p *Player) next(ctx context.Context, req mediaRequest, it *item, delta int) (*item, error) {
	path, _, _ := strings.Cut(it.queue, "?")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasPrefix(path, "/playQueues/") {
		return nil, errors.New("the item has no play queue")
	}
	var c containerXML
	if err := p.get(ctx, req, path, &c); err != nil {
		return nil, err
	}
	all := append(append([]mediaXML{}, c.Videos...), c.Tracks...)
	at := -1
	for i, m := range all {
		if m.RatingKey == it.ratingKey {
			at = i
			break
		}
	}
	if at < 0 {
		return nil, errors.New("the item is not in its queue")
	}
	to := at + delta
	if to < 0 || to >= len(all) {
		return nil, errors.New("the queue has no such item")
	}
	nreq := req
	nreq.key = or(all[to].Key, "/library/metadata/"+all[to].RatingKey)
	n, err := p.metadata(ctx, nreq)
	if err != nil {
		return nil, err
	}
	n.queue = it.queue
	return n, nil
}

// putPart tells the server which streams were chosen, for the whole item.
func (p *Player) putPart(req mediaRequest, partID, audio, subtitle int) {
	q := url.Values{"allParts": {"1"}}
	if audio >= 0 {
		q.Set("audioStreamID", strconv.Itoa(audio))
	}
	if subtitle >= 0 {
		q.Set("subtitleStreamID", strconv.Itoa(subtitle))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPut,
		req.server()+"/library/parts/"+strconv.Itoa(partID)+"?"+q.Encode(), nil)
	if err != nil {
		return
	}
	token := req.token
	if p.ServerToken != nil {
		if t := p.ServerToken(ctx, req.machine, r.URL.Hostname()); t != "" {
			token = t
		}
	}
	p.serverHeaders(r, token)
	resp, err := p.Fetch.Do(r)
	if err != nil {
		slog.Debug("plex: the server was not told about the stream choice", "err", scrub(err))
		return
	}
	slog.Debug("plex: told the server the stream choice", "status", resp.StatusCode, "audio", audio, "subtitle", subtitle)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
}

// report tells the server where the player is: key and ratingKey both, because a report with only one
// is answered 200 and quietly does nothing.
func (p *Player) report() {
	p.mu.Lock()
	s := p.cur
	if s == nil || s.req.address == "" {
		p.mu.Unlock()
		return
	}
	req, it, session := s.req, s.it, s.id
	pos, st := s.position(time.Now()), s.state
	s.reported = time.Now()
	p.mu.Unlock()
	if st == stateError {
		st = stateStopped
	}
	p.sendReport(req, it, session, st, pos)
}

// sendReport is one report to the item's server: the item, its state and where it is.
func (p *Player) sendReport(req mediaRequest, it *item, session, st string, pos time.Duration) {
	q := url.Values{
		"key":        {it.key},
		"ratingKey":  {it.ratingKey},
		"state":      {st},
		"time":       {strconv.FormatInt(pos.Milliseconds(), 10)},
		"duration":   {strconv.FormatInt(it.dur.Milliseconds(), 10)},
		"identifier": {"com.plexapp.plugins.library"},
	}
	if it.queue != "" {
		q.Set("containerKey", it.queue)
	}
	if _, item := queueIDs(it.queue); item != "" {
		q.Set("playQueueItemID", item)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, req.server()+"/:/timeline?"+q.Encode(), nil)
	if err != nil {
		return
	}
	p.serverHeaders(r, req.token)
	r.Header.Set("X-Plex-Session-Identifier", session)
	r.Header.Set("X-Plex-Platform", plextv.Platform)
	resp, err := p.Fetch.Do(r)
	if err != nil {
		slog.Debug("plex: the server was not told where the player is", "err", scrub(err))
		return
	}
	slog.Debug("plex: told the server", "status", resp.StatusCode, "state", st, "time", pos.Truncate(time.Second))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
}
