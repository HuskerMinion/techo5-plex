package companion

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/dlna"
	"github.com/HuskerMinion/techo5-plex/internal/netguard"
	"github.com/HuskerMinion/techo5-plex/internal/redact"
	"github.com/HuskerMinion/techo5-plex/internal/state"
)

// The states a Plex timeline names.
const (
	statePlaying   = "playing"
	statePaused    = "paused"
	stateStopped   = "stopped"
	stateBuffering = "buffering"
	stateError     = "error"
)

var errNothingPlaying = errors.New("nothing is playing")

// session is the item the player has, and what was handed to the renderer for it.
type session struct {
	gen     int
	req     mediaRequest
	it      *item
	id      string // X-Plex-Session-Identifier: one per stream handed over
	stream  streamURL
	state   string
	pos     time.Duration // where in the item, as last read
	posAt   time.Time
	started time.Time // when the stream was handed over
	played  bool      // the renderer has said PLAYING for this stream
	owned   bool      // the renderer is still playing our stream

	subtitles       bool // burned in, for a film
	shuffle, repeat int
	queueID, itemID string
	queueVersion    string // the play queue's version, learned from the server (learnQueue)

	seekTimer *time.Timer
	reported  time.Time
}

// position is where the item is now: the last reading, moved on while it plays.
func (s *session) position(now time.Time) time.Duration {
	d := s.pos
	if s.state == statePlaying && !s.posAt.IsZero() {
		d += now.Sub(s.posAt)
	}
	if s.it != nil && s.it.dur > 0 && d > s.it.dur {
		d = s.it.dur
	}
	return max(d, 0)
}

// startGrace is how long a stream handed over may read STOPPED before it is taken as refused: the
// renderer sets the URL, then plays.
const startGrace = 5 * time.Second

// seekWait collects a burst of seeks (a scrub bar) into one new stream.
const seekWait = 600 * time.Millisecond

// reportEvery is how often the Plex server is told where the player is while it plays.
const reportEvery = 10 * time.Second

// playMedia is how a casting app starts something.
func (p *Player) playMedia(w http.ResponseWriter, r *http.Request) {
	if !readable(w, r) {
		return
	}
	q := query{r}
	req := mediaRequest{
		protocol:  or(q.get("protocol"), "http"),
		address:   q.get("address"),
		port:      or(q.get("port"), "32400"),
		key:       q.get("key"),
		token:     q.get("token"),
		machine:   q.get("machineIdentifier"),
		container: q.get("containerKey"),
		typ:       q.get("type"),
		offset:    q.millis("offset"),
	}
	redact.Secret(req.token)
	if err := req.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if ip := net.ParseIP(req.address); ip != nil && p.RefuseAddr != nil && p.RefuseAddr(ip) {
		fail(w, http.StatusBadRequest, "the server's address is not one this player fetches from")
		return
	}
	if p.OnServer != nil {
		ip := net.ParseIP(req.address)
		if ip == nil {
			ip = netguard.PlexDirectIP(req.address)
		}
		if ip != nil {
			p.OnServer(ip)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	it, err := p.metadata(ctx, req)
	if err != nil {
		slog.Warn("plex: the item could not be described", "key", req.key, "err", err)
		fail(w, http.StatusInternalServerError, "the server could not describe this item")
		return
	}
	if err := p.start(ctx, req, it, req.offset, nil); err != nil {
		slog.Warn("plex: the item could not be started", "title", it.title, "err", err)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("plex: playing", "title", it.title, "type", it.typ, "offset", req.offset)
	agree(w)
}

// start hands an item to the renderer from offset, in place of whatever was playing. keep carries a
// restart's settings (a seek, a stream choice); nil is a new item.
func (p *Player) start(ctx context.Context, req mediaRequest, it *item, offset time.Duration, keep *session) error {
	r, err := p.renderer()
	if err != nil {
		return err
	}
	if it.dur > 0 && offset >= it.dur {
		offset = 0
	}
	if it.video && !p.accountServer(ctx, req) {
		return errNotAccountServer
	}
	p.mu.Lock()
	p.gen++
	old := p.cur
	s := &session{
		gen: p.gen, req: req, it: it, id: state.NewUUID(), state: stateBuffering,
		pos: offset, started: time.Now(), owned: true,
	}
	s.queueID, s.itemID = queueIDs(req.container)
	if keep != nil {
		s.subtitles, s.shuffle, s.repeat = keep.subtitles, keep.shuffle, keep.repeat
		if keep.queueID != "" {
			s.queueID = keep.queueID
		}
	} else {
		s.subtitles = it.part.selected(streamSubtitle) != 0
		if old != nil {
			s.shuffle, s.repeat = old.shuffle, old.repeat
		}
	}
	if old != nil && old.seekTimer != nil {
		old.seekTimer.Stop()
	}
	p.cur = s
	p.mu.Unlock()
	if strings.HasPrefix(strings.TrimPrefix(it.queue, "/"), "playQueues/") {
		go p.learnQueue(s)
	}
	if old != nil && old.stream.transcoding {
		go p.stopTranscode(old.req, old.it.video, old.id)
	}

	var su streamURL
	if it.video {
		su, err = p.videoStream(ctx, req, it, offset, s.subtitles, s.id)
	} else {
		su = p.audioStream(req, it, offset, s.id)
	}
	if err == nil {
		meta := dlna.DIDL(dlna.Meta{
			Title: it.title, Artist: it.artist, Album: it.album, Art: artURL(req, it.thumb),
			Dur: it.dur - su.base, Start: su.base, Video: it.video, MIME: su.mime, URI: su.uri,
		})
		if err = r.SetURI(ctx, su.uri, meta); err == nil {
			err = r.Play(ctx)
		}
	}
	p.mu.Lock()
	if p.cur != s {
		p.mu.Unlock()
		return nil // overtaken by a later start
	}
	if err != nil {
		s.state, s.owned = stateError, false
	} else {
		s.stream, s.pos, s.posAt = su, su.base, time.Now()
	}
	p.mu.Unlock()
	p.changed()
	if err != nil {
		switch {
		case dlna.IsFault(err, dlna.FaultIllegalMIME):
			return errors.New("the device does not take video over DLNA: turn on Video and DLNA video in TECHO5")
		case dlna.IsFault(err, dlna.FaultTransition):
			return errors.New("the device would not play it")
		}
		return err
	}
	return nil
}

// current is the session, or nil.
func (p *Player) current() *session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

func (p *Player) play(ctx context.Context) error {
	r, err := p.renderer()
	if err != nil {
		return err
	}
	p.mu.Lock()
	s := p.cur
	if s == nil {
		p.mu.Unlock()
		return errNothingPlaying
	}
	switch {
	case s.state == statePaused && s.owned:
		p.mu.Unlock()
		if err := r.Play(ctx); err != nil {
			return err
		}
		p.mu.Lock()
		if p.cur == s {
			s.state, s.posAt = statePlaying, time.Now()
		}
		p.mu.Unlock()
		p.changed()
		return nil
	case s.state == stateStopped || s.state == stateError || !s.owned:
		// Where it stopped: a stop keeps its place.
		req, it, pos := s.req, s.it, s.position(time.Now())
		p.mu.Unlock()
		return p.start(ctx, req, it, pos, s)
	}
	p.mu.Unlock()
	return nil
}

func (p *Player) pause(ctx context.Context) error {
	r, err := p.renderer()
	if err != nil {
		return err
	}
	p.mu.Lock()
	s := p.cur
	if s == nil || !s.owned || (s.state != statePlaying && s.state != stateBuffering) {
		p.mu.Unlock()
		return errNothingPlaying
	}
	p.mu.Unlock()
	if err := r.Pause(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	if p.cur == s {
		s.pos, s.posAt, s.state = s.position(time.Now()), time.Now(), statePaused
	}
	p.mu.Unlock()
	p.changed()
	return nil
}

func (p *Player) stop(ctx context.Context) error {
	p.mu.Lock()
	s := p.cur
	if s == nil {
		p.mu.Unlock()
		return nil
	}
	if s.seekTimer != nil {
		s.seekTimer.Stop()
	}
	owned := s.owned
	s.pos, s.posAt = s.position(time.Now()), time.Now()
	s.state, s.owned = stateStopped, false
	p.mu.Unlock()
	if owned {
		if r, err := p.renderer(); err == nil {
			if err := r.Stop(ctx); err != nil {
				slog.Debug("plex: the renderer did not stop", "err", err)
			}
		}
	}
	if s.stream.transcoding {
		go p.stopTranscode(s.req, s.it.video, s.id)
	}
	p.changed()
	return nil
}

// seek moves to pos, after a short wait that collects a scrub's burst of seeks into one. The renderer
// is asked first (Seek REL_TIME into the stream it has). A renderer that answers 710 (an older TECHO5,
// or a stream it cannot seek in) gets a new stream that starts at pos instead: Plex's transcode with
// offset=.
func (p *Player) seek(_ context.Context, pos time.Duration) error {
	if _, err := p.renderer(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.cur
	if s == nil {
		return errNothingPlaying
	}
	if s.it.dur > 0 && pos > s.it.dur {
		pos = s.it.dur
	}
	s.pos, s.posAt = max(pos, 0), time.Now()
	if s.seekTimer != nil {
		s.seekTimer.Stop()
	}
	s.seekTimer = time.AfterFunc(seekWait, func() { p.seekNow(s) })
	return nil
}

// seekNow carries out the last seek asked for.
func (p *Player) seekNow(s *session) {
	r, err := p.renderer()
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.cur != s {
		p.mu.Unlock()
		return
	}
	req, it, pos, base, owned := s.req, s.it, s.pos, s.stream.base, s.owned
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if owned && pos >= base {
		err := r.Seek(ctx, pos-base)
		if err == nil {
			p.mu.Lock()
			if p.cur == s {
				s.pos, s.posAt, s.seekTimer = pos, time.Now(), nil
			}
			p.mu.Unlock()
			p.changed()
			return
		}
		if !dlna.IsFault(err, dlna.FaultSeekMode) {
			p.mu.Lock()
			if p.cur == s {
				s.seekTimer = nil
			}
			p.mu.Unlock()
			slog.Warn("plex: the renderer would not seek", "err", err)
			return
		}
	}
	if err := p.start(ctx, req, it, pos, s); err != nil {
		slog.Warn("plex: seeking failed", "err", err)
	}
}

func (p *Player) step(ctx context.Context, d time.Duration) error {
	s := p.current()
	if s == nil {
		return errNothingPlaying
	}
	p.mu.Lock()
	pos := s.position(time.Now())
	p.mu.Unlock()
	return p.seek(ctx, pos+d)
}

func (p *Player) skip(ctx context.Context, delta int) error {
	return p.skipFrom(ctx, p.current(), delta)
}

// skipFrom goes delta items on in s's queue.
func (p *Player) skipFrom(ctx context.Context, s *session, delta int) error {
	if s == nil || s.it.queue == "" {
		return errNothingPlaying
	}
	next, err := p.next(ctx, s.req, s.it, delta)
	if err != nil {
		return err
	}
	req := s.req
	req.key, req.offset = next.key, 0
	return p.start(ctx, req, next, 0, s)
}

// setStreams chooses the sound and the subtitles, -1 for "as it is" and 0 for none of that kind. The
// server records the choice; a film is restarted at the same second with it, since a track cannot be
// switched inside a stream being made.
func (p *Player) setStreams(ctx context.Context, audio, subtitle int) error {
	p.mu.Lock()
	s := p.cur
	if s == nil {
		p.mu.Unlock()
		return errNothingPlaying
	}
	if audio >= 0 {
		s.it.part.selectStream(streamAudio, audio)
	}
	if subtitle >= 0 {
		s.it.part.selectStream(streamSubtitle, subtitle)
		s.subtitles = subtitle != 0
	}
	req, it, pos, video := s.req, s.it, s.position(time.Now()), s.it.video
	p.mu.Unlock()
	if it.part.id != 0 {
		if !video {
			go p.putPart(req, it.part.id, audio, subtitle)
			return nil
		}
		// The server makes the new stream with the tracks it has on record for the part, so the
		// choice is recorded before the film restarts, not alongside.
		p.putPart(req, it.part.id, audio, subtitle)
	}
	if !video {
		return nil
	}
	slog.Debug("plex: restarting with other tracks", "audio", it.part.selected(streamAudio), "subtitle", it.part.selected(streamSubtitle))
	return p.start(ctx, req, it, pos, s)
}

// setParameters is the volume (the device's own, through RenderingControl), shuffle and repeat.
func (p *Player) setParameters(w http.ResponseWriter, r *http.Request) {
	if !readable(w, r) {
		return
	}
	q := query{r}
	if v := q.get("volume"); v != "" {
		pct := q.number("volume", -1)
		if pct < 0 || pct > 100 {
			fail(w, http.StatusBadRequest, "volume is 0-100")
			return
		}
		rd, err := p.renderer()
		if err != nil {
			fail(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = rd.SetVolume(ctx, pct)
		cancel()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		p.mu.Lock()
		p.vol, p.volAt = pct, time.Now()
		p.mu.Unlock()
	}
	shuffle, repeat := q.number("shuffle", -1), q.number("repeat", -1)
	p.mu.Lock()
	if s := p.cur; s != nil {
		if shuffle >= 0 {
			s.shuffle = min(shuffle, 1)
		}
		if repeat >= 0 {
			s.repeat = min(repeat, 2)
		}
	}
	p.mu.Unlock()
	p.tl.push()
	agree(w)
}

// refreshPlayQueue notes a queue the controller changed, so a skip walks the new one.
func (p *Player) refreshPlayQueue(w http.ResponseWriter, r *http.Request) {
	if !readable(w, r) {
		return
	}
	if id := (query{r}).get("playQueueID"); id != "" {
		p.mu.Lock()
		if s := p.cur; s != nil {
			s.queueID = id
		}
		p.mu.Unlock()
		p.tl.push()
	}
	agree(w)
}

// changed is a state change controllers should hear at once.
func (p *Player) changed() {
	p.tl.push()
	go p.report()
}

// Run is the clock: once a second while something is the player's, the renderer is asked what it is
// doing, the timeline is pushed, and the server told every ten seconds. Idle, it reads the volume now
// and then and does nothing else.
func (p *Player) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		case <-p.nudge:
			p.tick(ctx)
		}
	}
}

func (p *Player) tick(ctx context.Context) {
	p.refreshVolume(ctx)
	p.mu.Lock()
	s := p.cur
	idle := s == nil || !s.owned || s.seekTimer != nil
	p.mu.Unlock()
	if idle {
		return
	}
	r, err := p.renderer()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := r.TransportState(ctx)
	if err != nil {
		slog.Debug("plex: the renderer's state could not be read", "err", err)
		return
	}
	pos, err := r.Position(ctx)
	if err != nil {
		slog.Debug("plex: the renderer's position could not be read", "err", err)
		return
	}
	advance := p.observe(s, st, pos, time.Now())
	if advance {
		if err := p.skip(ctx, 1); err != nil {
			slog.Debug("plex: nothing to go on to", "err", err)
		}
		return
	}
	p.mu.Lock()
	playing := p.cur == s && s.state == statePlaying
	due := playing && time.Since(s.reported) >= reportEvery
	p.mu.Unlock()
	if playing {
		p.tl.push()
	}
	if due {
		go p.report()
	}
}

// observe folds one reading of the renderer into the session, and says whether the item played to its
// end (and the next in the queue should start).
func (p *Player) observe(s *session, st string, pos dlna.Position, now time.Time) (advance bool) {
	p.mu.Lock()
	if p.cur != s || !s.owned || s.seekTimer != nil || s.stream.uri == "" {
		// A session still handing its stream to the renderer has nothing to compare with yet: what
		// the renderer plays then is the stream before (a seek), not someone else's.
		p.mu.Unlock()
		return false
	}
	was := s.state
	if pos.URI != "" && pos.URI != s.stream.uri {
		// Something else has the renderer now (another cast, a station): the item stays, stopped.
		slog.Debug("plex: the renderer has another stream", "len", len(pos.URI), "ours", len(s.stream.uri))
		s.pos, s.posAt = s.position(now), now
		s.state, s.owned = stateStopped, false
		p.mu.Unlock()
		p.changed()
		return false
	}
	switch st {
	case dlna.Playing:
		s.state, s.played = statePlaying, true
		s.pos, s.posAt = s.stream.base+pos.Rel, now
	case dlna.Paused:
		s.state = statePaused
		s.pos, s.posAt = s.stream.base+pos.Rel, now
	case dlna.Transitioning:
		s.state = stateBuffering
	case dlna.Stopped, dlna.NoMedia:
		if !s.played {
			if now.Sub(s.started) < startGrace {
				break
			}
			s.state, s.owned = stateError, false
			break
		}
		last := s.position(now)
		s.state, s.owned = stateStopped, false
		s.pos, s.posAt = last, now
		ended := s.it.dur == 0 || last >= s.it.dur-10*time.Second
		advance = ended && s.it.queue != "" && s.repeat != 1
	}
	changed := s.state != was
	if changed {
		slog.Debug("plex: the renderer says", "transport", st, "state", s.state, "rel", pos.Rel)
	}
	p.mu.Unlock()
	if changed && !advance {
		p.changed()
	}
	return advance
}

// Event is one of the renderer's AVTransport events. Next or Previous pressed on the device's own Now
// Playing (dlna.RequestVar) is a skip, as a controller's skipNext and skipPrevious are, and a place
// chosen on its screen's bar is a seek, as a controller's seekTo is, while the renderer is playing
// what this player handed it. A change of state is read at once rather than at
// the next second, so controllers hear it sooner.
func (p *Player) Event(ev dlna.Event) {
	if ev.Request == "seek" {
		p.mu.Lock()
		s := p.cur
		ours := s != nil && s.owned
		if ours {
			if uri := ev.Vars["CurrentTrackURI"]; uri != "" && uri != s.stream.uri {
				ours = false
			}
		}
		p.mu.Unlock()
		if !ours {
			slog.Debug("plex: a seek on the device for something not ours")
			return
		}
		slog.Info("plex: seek from the device", "to", ev.At)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if p.current() != s {
				return // overtaken by another cast
			}
			if err := p.seek(ctx, ev.At); err != nil {
				slog.Warn("plex: the seek from the device failed", "err", err)
			}
		}()
		return
	}
	if ev.Request == "next" || ev.Request == "previous" {
		delta := 1
		if ev.Request == "previous" {
			delta = -1
		}
		p.mu.Lock()
		s := p.cur
		ours := s != nil && s.owned && s.it.queue != ""
		if ours {
			if uri := ev.Vars["CurrentTrackURI"]; uri != "" && uri != s.stream.uri {
				ours = false
			}
		}
		p.mu.Unlock()
		if !ours {
			slog.Debug("plex: a skip on the device for something not ours", "request", ev.Request)
		} else {
			slog.Info("plex: skip from the device", "request", ev.Request)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if p.current() != s {
					return // overtaken by another cast
				}
				if err := p.skipFrom(ctx, s, delta); err != nil {
					slog.Warn("plex: the skip from the device failed", "err", err)
				}
			}()
			return
		}
	}
	if ev.TransportState() != "" {
		select {
		case p.nudge <- struct{}{}:
		default:
		}
	}
}

// refreshVolume reads the renderer's volume every ten seconds, for the timeline's slider.
func (p *Player) refreshVolume(ctx context.Context) {
	p.mu.Lock()
	due := time.Since(p.volAt) >= 10*time.Second
	if due {
		p.volAt = time.Now()
	}
	p.mu.Unlock()
	if !due {
		return
	}
	r, err := p.renderer()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	v, err := r.Volume(ctx)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.vol = v
	p.mu.Unlock()
}

// queueIDs is the play queue's number and the item's, from the containerKey a controller sends
// (/playQueues/12?window=100&own=1).
func queueIDs(container string) (queue, item string) {
	path, q, _ := strings.Cut(container, "?")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		queue = path[i+1:]
	}
	for _, kv := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k == "playQueueItemID" {
			item = v
		}
	}
	return queue, item
}

// learnQueue asks the server for the play queue s's item came in, for what controllers need to show
// it: which entry is playing (playQueueItemID) and the queue's version. playMedia names the queue but
// not the entry, and without them Plexamp shows no "Up Next" and offers no next or previous.
func (p *Player) learnQueue(s *session) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	path, _, _ := strings.Cut(s.it.queue, "?")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var c containerXML
	if err := p.get(ctx, s.req, path+"?window=200&own=1", &c); err != nil {
		slog.Debug("plex: the play queue could not be read", "err", err)
		return
	}
	item := ""
	for _, m := range append(append([]mediaXML{}, c.Videos...), c.Tracks...) {
		if m.RatingKey != s.it.ratingKey {
			continue
		}
		if item == "" || m.PlayQueueItemID == c.PlayQueueSelectedItemID {
			item = m.PlayQueueItemID
		}
	}
	p.mu.Lock()
	if p.cur != s {
		p.mu.Unlock()
		return
	}
	if s.itemID == "" {
		s.itemID = item
	}
	s.queueVersion = c.PlayQueueVersion
	p.mu.Unlock()
	slog.Debug("plex: play queue read", "item", item, "version", c.PlayQueueVersion)
	p.changed()
}
