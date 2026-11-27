package companion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/HuskerMinion/techo5-plex/internal/state"
)

// Local plays films and episodes from the linked account's servers on this device itself, through the
// add-on API's media ops (TECHO5's own video player), for voice: no Plex app and no DLNA involved. The
// stream is the one a cast gets (videoStream: the server's decision for the device's profile, copying
// what the device decodes and converting the rest), and the server is told where it is, as for a cast,
// so a film's place is kept and an episode is marked watched.
//
// Where the item is, and how it ended, is the device's word: the API's media.state events (State),
// which the server's reports follow. One stopped on the device, replaced or failed is reported stopped
// where it was, so the server applies its own rule (watched past about 90%); one played to its end is
// reported at its end, and so marked watched.

// Media is the device's player, as the add-on API offers it. An op on something the add-on no longer
// plays answers ErrGone.
type Media interface {
	// PlayVideo plays a video and answers the id the device's states of it carry.
	PlayVideo(ctx context.Context, url, title string) (string, error)
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Stop(ctx context.Context) error
}

// ErrGone is what Media answers for an item the device no longer plays for the add-on.
var ErrGone = errors.New("the device is not playing it any more")

// ErrNothingPlaying is a pause, resume or stop with nothing of this player's on the device.
var ErrNothingPlaying = errors.New("nothing from Plex is playing")

// Source is a Plex server as voice reaches it: Base is http://address:port on the home network, and
// Token the one the add-on uses on it, which also goes in the stream's address for the device's
// player. A transient token (the server's /security/token), so the account's own never leaves the
// add-on.
type Source struct {
	Base    string
	Machine string
	Token   string
}

func (s Source) request(key string) (mediaRequest, error) {
	u, err := url.Parse(s.Base)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return mediaRequest{}, errors.New("the server's address is not usable")
	}
	port := u.Port()
	if port == "" {
		port = "32400"
	}
	req := mediaRequest{protocol: u.Scheme, address: u.Hostname(), port: port, key: key, token: s.Token, machine: s.Machine}
	return req, req.check()
}

// Item is a film or an episode, as voice speaks of it.
type Item struct {
	Key, RatingKey  string
	Type            string // movie, episode
	Title           string
	Year            int
	Show, ShowKey   string // an episode's show and its ratingKey
	Season, Episode int
	Duration        time.Duration
	ViewOffset      time.Duration // where it was left, 0 for not started or finished
	Watched         bool
}

func exported(it *item) Item {
	return Item{Key: it.key, RatingKey: it.ratingKey, Type: it.typ, Title: it.title, Year: it.year,
		Show: it.show, ShowKey: it.showKey, Season: it.season, Episode: it.episode, Duration: it.dur,
		ViewOffset: it.viewOffset, Watched: it.watched}
}

// Describe is the film or episode at key on src.
func (p *Player) Describe(ctx context.Context, src Source, key string) (Item, error) {
	req, err := src.request(key)
	if err != nil {
		return Item{}, err
	}
	it, err := p.metadata(ctx, req)
	if err != nil {
		return Item{}, err
	}
	return exported(it), nil
}

// Episodes is every episode of the show showKey (a ratingKey) on src, in the order the server keeps
// them: by season, then by number.
func (p *Player) Episodes(ctx context.Context, src Source, showKey string) ([]Item, error) {
	if showKey == "" || strings.ContainsAny(showKey, "/?#&% ") {
		return nil, errors.New("no such show")
	}
	req, err := src.request("/library/metadata/" + showKey + "/allLeaves")
	if err != nil {
		return nil, err
	}
	var c containerXML
	if err := p.get(ctx, req, req.key, &c); err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(c.Videos))
	for _, m := range c.Videos {
		if m.Type == "episode" {
			out = append(out, exported(fromXML(m, true)))
		}
	}
	return out, nil
}

// MediaState is what the device says its player is doing with something the add-on started: the
// API's media.state.
type MediaState struct {
	Media  string // the id PlayVideo answered
	State  string // buffering, playing, paused, stopped; idle for nothing started
	Reason string // with stopped: ended, device, replaced, error or addon
	// Position is how far into the stream the device is, when HasPosition; the stream starts where the
	// item was started from.
	Position    time.Duration
	HasPosition bool
}

// The reasons a stopped MediaState gives that matter here.
const (
	MediaEnded = "ended"
	MediaIdle  = "idle"
)

// Local is the player for what voice plays on the device.
type Local struct {
	p     *Player
	media Media
	now   func() time.Time

	mu    sync.Mutex
	cur   *localSession
	prev  map[string]*localSession // ones played before cur, until the device says how they ended
	early map[string]MediaState    // states of an id not yet answered by PlayVideo, the latest of each
	seq   uint64                   // Plays so far
	told  string                   // the state OnState was last told

	// OnState, when set, is told what the local player's item is doing whenever that changes:
	// "buffering", "playing", "paused", or "" for nothing. It is called outside the player's lock.
	OnState func(state string)
	reports chan localReport
}

type localSession struct {
	req         mediaRequest
	it          *item
	id          string // X-Plex-Session-Identifier
	media       string // the device's id for it
	base        time.Duration
	transcoding bool
	state       string        // buffering, playing or paused
	pos         time.Duration // in the item, as the device last said
	reported    time.Time
	stopping    bool   // stopped by a command; the device's word of it is awaited
	seq         uint64 // which Play it was
}

type localReport struct {
	req   mediaRequest
	it    *item
	id    string
	state string
	pos   time.Duration
	stop  bool // and end the server's stream
}

// NewLocal is a local player on p's identity and profile, playing through m. It learns what the device
// does with what it plays from State.
func NewLocal(p *Player, m Media) *Local {
	return &Local{p: p, media: m, now: time.Now, prev: map[string]*localSession{}, early: map[string]MediaState{}, reports: make(chan localReport, 16)}
}

// Title is what the device shows for an item: "The Office: S2 E4 Halloween", "Inception".
func Title(it Item) string {
	t := it.Title
	if it.Type == "episode" && it.Show != "" {
		t = fmt.Sprintf("%s: S%d E%d %s", it.Show, it.Season, it.Episode, it.Title)
	}
	t = strings.TrimSpace(t)
	if utf8.RuneCountInString(t) > 100 {
		r := []rune(t)
		t = string(r[:99]) + "…"
	}
	return t
}

// Resume is the offset of a Play from where the item was left.
const Resume time.Duration = -1

// Play has the device play the film or episode at key on src, from offset (Resume: where it was left).
func (l *Local) Play(ctx context.Context, src Source, key string, offset time.Duration) (Item, error) {
	req, err := src.request(key)
	if err != nil {
		return Item{}, err
	}
	it, err := l.p.metadata(ctx, req)
	if err != nil {
		return Item{}, err
	}
	if !it.video {
		return Item{}, errors.New("it is not a film or an episode")
	}
	if offset == Resume {
		offset = it.viewOffset
	}
	if offset < 0 || (it.dur > 0 && offset >= it.dur) {
		offset = 0
	}
	it.viewOffset = offset // in the answer: where it started
	id := state.NewUUID()
	su, err := l.p.videoStream(ctx, req, it, offset, it.part.selected(streamSubtitle) != 0, id)
	if err != nil {
		return Item{}, err
	}
	media, err := l.media.PlayVideo(ctx, su.uri, Title(exported(it)))
	if err != nil {
		if su.transcoding {
			go l.p.stopTranscode(req, true, id)
		}
		return Item{}, err
	}
	s := &localSession{req: req, it: it, id: id, media: media, base: su.base, transcoding: su.transcoding,
		state: stateBuffering, pos: su.base, reported: l.now()}
	var evicted []*localSession
	l.mu.Lock()
	l.seq++
	s.seq = l.seq
	if old := l.cur; old != nil {
		// Replaced by this one: its end, with where it was, is on its way.
		if len(l.prev) >= 4 {
			for k, o := range l.prev {
				evicted = append(evicted, o)
				delete(l.prev, k)
			}
		}
		l.prev[old.media] = old
	}
	l.cur = s
	early, ok := l.early[media]
	clear(l.early)
	l.mu.Unlock()
	for _, o := range evicted {
		l.end(o, o.pos, true)
	}
	l.report(localReport{req: req, it: it, id: id, state: stateBuffering, pos: su.base})
	if ok {
		l.state(early)
	}
	l.notify()
	slog.Debug("plex: voice plays", "type", it.typ, "offset", su.base)
	return exported(it), nil
}

// Current is what the local player has, and whether it plays ("playing", "buffering") or is paused
// ("paused"); "" for nothing.
func (l *Local) Current() (Item, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur == nil || l.cur.stopping {
		return Item{}, ""
	}
	return exported(l.cur.it), l.cur.state
}

// Pause pauses what the local player plays.
func (l *Local) Pause(ctx context.Context) error { return l.op(ctx, l.media.Pause) }

// Resume carries on with what the local player paused.
func (l *Local) Resume(ctx context.Context) error { return l.op(ctx, l.media.Resume) }

// Stop stops what the local player plays; the server keeps its place, told it by the device's word
// that it stopped.
func (l *Local) Stop(ctx context.Context) error {
	s := l.session()
	if s == nil {
		return ErrNothingPlaying
	}
	err := l.media.Stop(ctx)
	if err != nil {
		return l.failed(s, err)
	}
	l.mu.Lock()
	s.stopping = true
	l.mu.Unlock()
	l.notify()
	return nil
}

// op is a pause or a resume: the device's states say what came of it.
func (l *Local) op(ctx context.Context, fn func(context.Context) error) error {
	s := l.session()
	if s == nil {
		return ErrNothingPlaying
	}
	if err := fn(ctx); err != nil {
		return l.failed(s, err)
	}
	return nil
}

func (l *Local) session() *localSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur == nil || l.cur.stopping {
		return nil
	}
	return l.cur
}

// failed is an op refused: one the device no longer plays is over, where it was last said to be.
func (l *Local) failed(s *localSession, err error) error {
	if !errors.Is(err, ErrGone) {
		return err
	}
	l.mu.Lock()
	ours := l.cur == s
	if ours {
		l.cur = nil
	}
	l.mu.Unlock()
	if ours {
		l.end(s, s.pos, true)
	}
	l.notify()
	return ErrNothingPlaying
}

// end tells the server the item stopped at pos, and ends its stream.
func (l *Local) end(s *localSession, pos time.Duration, stop bool) {
	l.report(localReport{req: s.req, it: s.it, id: s.id, state: stateStopped, pos: pos, stop: stop && s.transcoding})
}

// State is the device saying what it does with something the add-on played: the server is told on each
// change, and every ten seconds of a position while it plays.
func (l *Local) State(st MediaState) {
	l.state(st)
	l.notify()
}

func (l *Local) state(st MediaState) {
	l.mu.Lock()
	if o, ok := l.prev[st.Media]; ok && st.Media != "" {
		o.at(st)
		if st.State == stateStopped {
			delete(l.prev, st.Media)
		}
		l.mu.Unlock()
		if st.State == stateStopped {
			l.stopped(o, st)
		}
		return
	}
	s := l.cur
	if s == nil || st.Media != s.media {
		if st.Media != "" && len(l.early) < 8 {
			// Possibly the one being started, its id not yet answered: kept for Play.
			l.early[st.Media] = st
		}
		l.mu.Unlock()
		return
	}
	now := l.now()
	s.at(st)
	if st.State == stateStopped {
		l.cur = nil
		l.mu.Unlock()
		l.stopped(s, st)
		return
	}
	switch st.State {
	case statePlaying, statePaused, stateBuffering:
	default:
		l.mu.Unlock()
		return
	}
	changed := st.State != s.state
	s.state = st.State
	due := changed || (st.State == statePlaying && st.HasPosition && now.Sub(s.reported) >= reportEvery-time.Second)
	if due {
		s.reported = now
	}
	r := localReport{req: s.req, it: s.it, id: s.id, state: s.state, pos: s.pos}
	l.mu.Unlock()
	if due {
		l.report(r)
	}
}

// at takes the position the device says, if it says one.
func (s *localSession) at(st MediaState) {
	if !st.HasPosition {
		return
	}
	s.pos = s.base + st.Position
	if s.it.dur > 0 && s.pos > s.it.dur {
		s.pos = s.it.dur
	}
}

// stopped reports s's end as the device said it: at its end if it played to it (so it is marked
// watched), else where it was, for the server's own rule.
func (l *Local) stopped(s *localSession, st MediaState) {
	pos := s.pos
	ended := st.Reason == MediaEnded
	if ended && s.it.dur > 0 {
		pos = s.it.dur
	}
	slog.Debug("plex: voice's item stopped", "reason", st.Reason, "at", pos.Truncate(time.Second))
	l.end(s, pos, !ended)
}

// Mark is where the plays have got to, for a Resync: taken before media.status is asked.
func (l *Local) Mark() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Resync is what the device says after the add-on connects to it again (media.status, asked after
// Mark answered mark). Only what was played before then is held to it: a play since is newer than the
// answer. The item, if it is the one the device speaks of, goes on from there; anything else played
// before is over, where it was last said to be.
func (l *Local) Resync(st MediaState, mark uint64) {
	defer l.notify()
	l.mu.Lock()
	s := l.cur
	var gone []*localSession
	for k, o := range l.prev {
		if o.seq <= mark && k != st.Media {
			gone = append(gone, o)
			delete(l.prev, k)
		}
	}
	if s != nil && s.seq > mark {
		s = nil
	}
	l.mu.Unlock()
	for _, o := range gone {
		l.end(o, o.pos, true)
	}
	if st.Media != "" {
		l.state(st) // the current one, or a replaced one's end
	}
	if s == nil || st.Media == s.media {
		return
	}
	l.mu.Lock()
	ours := l.cur == s
	if ours {
		l.cur = nil
	}
	l.mu.Unlock()
	if ours {
		l.end(s, s.pos, true)
	}
}

// notify tells OnState of a change in what the item is doing.
func (l *Local) notify() {
	_, st := l.Current()
	l.mu.Lock()
	changed := st != l.told
	l.told = st
	fn := l.OnState
	l.mu.Unlock()
	if changed && fn != nil {
		fn(st)
	}
}

func (l *Local) report(r localReport) {
	select {
	case l.reports <- r:
	default: // the server is slow to answer: a report is dropped rather than kept waiting
	}
}

// Run sends the server the reports, one at a time and in order, until ctx ends.
func (l *Local) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-l.reports:
			l.p.sendReport(r.req, r.it, r.id, r.state, r.pos)
			if r.stop {
				l.p.stopTranscode(r.req, true, r.id)
			}
		}
	}
}
