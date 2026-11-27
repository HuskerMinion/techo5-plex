// Package voice is Plex by voice on the device: the commands TECHO5's add-on API carries from Home
// Assistant (play a film or a show by its title, an episode by its numbers, pause, resume, stop, the
// next episode), answered with a sentence Home Assistant can say, and played on the device itself
// through the API's media capability (companion.Local).
//
// Nothing is written to the flash for a command: the library is kept in memory, the transient tokens
// too, and what is logged for a command is at debug level only (an add-on's log is a file on the
// flash).
package voice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/HuskerMinion/techo5-plex/internal/companion"
	"github.com/HuskerMinion/techo5-plex/internal/plextv"
	"github.com/HuskerMinion/techo5-plex/internal/redact"
)

// Source is a server as the player plays from it.
type Source = companion.Source

var secret = redact.Secret

// The commands, as addon.json declares them.
const (
	CmdPlay        = "play"
	CmdPlayEpisode = "play_episode"
	CmdPause       = "pause"
	CmdResume      = "resume"
	CmdStop        = "stop"
	CmdNextEpisode = "next_episode"
	CmdRestart     = "restart"
	CmdChoose      = "choose"

	// ArgIfKnown on play: play only a title the library has, and otherwise answer nothing.
	ArgIfKnown = "if_known"

	// The entities, as addon.json declares them: on while a question waits for its answer, and while
	// what voice plays is paused.
	EntityAsking = "asking"
	EntityPaused = "paused"
)

// refreshEvery is how often the library is read again, and staleAfter how old it may be before a title
// not found in it has it read again at once.
const (
	refreshEvery = time.Hour
	staleAfter   = 2 * time.Minute
)

// Voice answers the commands.
type Voice struct {
	// Token is the linked account's token, "" while none is linked.
	Token func() string
	// Servers lists the account's servers on the home network (plextv.Client.Servers).
	Servers func(ctx context.Context, token string) ([]plextv.Server, error)
	// Player is the Companion player, whose identity, profile and streams voice shares.
	Player *companion.Player
	// HTTP is for the Plex servers (netguard.Client: never the device itself).
	HTTP *http.Client

	local   *companion.Local
	lib     library
	reading atomic.Bool // the library is being read in the background (readSoon)

	tmu        sync.Mutex
	transients map[string]transient // by server machine

	mu   sync.Mutex // one command at a time
	from *server    // the server what plays came from
	last *lastPlay  // what voice played last, for restart

	pmu     sync.Mutex
	pending *choice // a question waiting for its answer
	states  Entities
	shown   map[string]bool // what the entities were last set to
}

// Entities sets the add-on's entities in Home Assistant (the API's entity.set).
type Entities interface {
	SetEntity(ctx context.Context, id string, state any) error
}

// choice is a question asked: an item part watched, to resume or to start over.
type choice struct {
	it   companion.Item
	name string
	srv  *server
	at   time.Duration
}

// AskFor is how long a question waits for its answer.
var AskFor = 30 * time.Second

// New is voice for the player, playing through media.
func New(p *companion.Player, media companion.Media, token func() string, servers func(context.Context, string) ([]plextv.Server, error), hc *http.Client) *Voice {
	v := &Voice{Token: token, Servers: servers, Player: p, HTTP: hc, local: companion.NewLocal(p, media),
		shown: map[string]bool{}}
	if e, ok := media.(Entities); ok {
		v.states = e
	}
	v.local.OnState = func(st string) { v.set(EntityPaused, st == "paused") }
	return v
}

// set sets an entity, when it changes, and keeps what it was set to for Publish.
func (v *Voice) set(id string, on bool) {
	v.pmu.Lock()
	v.shown[id] = on
	e := v.states
	v.pmu.Unlock()
	if e == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.SetEntity(ctx, id, on); err != nil {
		slog.Debug("plex: an entity was not set", "id", id, "err", err)
	}
}

// Publish sets every entity again, as a new connection to the device needs (states are not kept).
func (v *Voice) Publish() {
	v.pmu.Lock()
	asking, paused := v.pending != nil, v.shown[EntityPaused]
	v.pmu.Unlock()
	v.set(EntityAsking, asking)
	v.set(EntityPaused, paused)
}

// ask records a question and has the asking entity say so, for AskFor.
func (v *Voice) ask(c *choice) {
	v.pmu.Lock()
	v.pending = c
	v.pmu.Unlock()
	v.set(EntityAsking, true)
	time.AfterFunc(AskFor, func() {
		v.pmu.Lock()
		mine := v.pending == c
		if mine {
			v.pending = nil
		}
		v.pmu.Unlock()
		if mine {
			v.set(EntityAsking, false)
		}
	})
}

// answered takes the question waiting, if there is one.
func (v *Voice) answered() *choice {
	v.pmu.Lock()
	c := v.pending
	v.pending = nil
	v.pmu.Unlock()
	if c != nil {
		v.set(EntityAsking, false)
	}
	return c
}

func (v *Voice) asking() bool {
	v.pmu.Lock()
	defer v.pmu.Unlock()
	return v.pending != nil
}

// Run keeps the library read and the local player's reports going until ctx ends: on link, every
// hour, and a minute after a reading that failed.
func (v *Voice) Run(ctx context.Context) {
	go v.local.Run(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	var next time.Time // no reading before this, after one failed
	for {
		token := v.Token()
		v.lib.mu.Lock()
		was, at := v.lib.token, v.lib.at
		v.lib.mu.Unlock()
		switch {
		case token == "":
			if was != "" {
				v.forget()
			}
		case time.Now().After(next) && (token != was || time.Since(at) >= refreshEvery):
			secret(token)
			rctx, cancel := context.WithTimeout(ctx, time.Minute)
			if err := v.readLibrary(rctx, token); err != nil && ctx.Err() == nil {
				slog.Debug("plex: the library could not be read", "err", err)
				next = time.Now().Add(time.Minute)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// forget drops the library and the tokens of an account no longer linked.
func (v *Voice) forget() {
	v.lib.mu.Lock()
	v.lib.entries, v.lib.servers, v.lib.at, v.lib.token, v.lib.err = nil, nil, time.Time{}, "", nil
	v.lib.mu.Unlock()
	v.tmu.Lock()
	v.transients = nil
	v.tmu.Unlock()
}

// Answers.
const (
	sayNotLinked   = "Plex isn't linked yet. Open the Plex add-on's link page to link your account."
	sayNoServer    = "I can't reach your Plex server."
	sayNothing     = "Nothing from Plex is playing."
	sayWhat        = "What should I play from Plex?"
	sayPaused      = "Paused."
	sayResumed     = "Resuming."
	sayStopped     = "Stopped."
	sayDropped     = "OK, I won't start it."
	sayUnknownWhat = "The Plex add-on doesn't know that command."
)

// Command carries out one command, and answers what Home Assistant should say.
func (v *Voice) Command(ctx context.Context, name string, args map[string]any) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch name {
	case CmdPlay:
		title, over := fromStart(str(args, "title"), args)
		// "the office season 2 episode 4" said to play is play_episode: a sentence that took it all as
		// the title still plays the episode.
		if m := spokenEpisode.FindStringSubmatch(title); m != nil {
			season, ok1 := number(m[2])
			episode, ok2 := number(m[3])
			if ok1 && ok2 && strings.TrimSpace(m[1]) != "" {
				return v.playEpisode(ctx, m[1], season, episode, over)
			}
		}
		return v.play(ctx, title, over)
	case CmdPlayEpisode:
		season, ok1 := number(str(args, "season"))
		episode, ok2 := number(str(args, "episode"))
		if !ok1 || !ok2 {
			return "Which season and episode? Say them as numbers."
		}
		title, over := fromStart(str(args, "title"), args)
		return v.playEpisode(ctx, title, season, episode, over)
	case CmdPause:
		return v.transport(ctx, v.local.Pause, sayPaused)
	case CmdResume:
		// A question waiting is newer than anything paused: resume answers it.
		if v.asking() {
			return v.choose(ctx, "resume")
		}
		return v.transport(ctx, v.local.Resume, sayResumed)
	case CmdChoose:
		return v.choose(ctx, str(args, "answer"))
	case CmdStop:
		// Stop also drops a question waiting: what it asked about is not to be started.
		dropped := v.answered() != nil
		said := v.transport(ctx, v.local.Stop, sayStopped)
		if dropped && said == sayNothing {
			return sayDropped
		}
		return said
	case CmdNextEpisode:
		return v.nextEpisode(ctx)
	case CmdRestart:
		return v.restart(ctx)
	}
	return sayUnknownWhat
}

// Pending is what is said when a command is still at it after the time Home Assistant waits.
func Pending(name string, args map[string]any) string {
	switch name {
	case CmdPlay, CmdPlayEpisode:
		if t := strings.TrimSpace(str(args, "title")); t != "" {
			return "Looking for " + t + " on Plex."
		}
	}
	return "Working on it."
}

func (v *Voice) transport(ctx context.Context, op func(context.Context) error, done string) string {
	if err := op(ctx); err != nil {
		if errors.Is(err, companion.ErrNothingPlaying) {
			return sayNothing
		}
		return mediaProblem(err)
	}
	return done
}

// entries is the library: read now if it never was for this account, or, with again, if it is older
// than staleAfter. A stale list is still matched against when the server does not answer.
func (v *Voice) entries(ctx context.Context, token string, again bool) ([]*Entry, error) {
	entries, at, _ := v.lib.snapshot()
	v.lib.mu.Lock()
	read := v.lib.token == token && !at.IsZero()
	v.lib.mu.Unlock()
	if !read || (again && time.Since(at) >= staleAfter) {
		if err := v.readLibrary(ctx, token); err != nil {
			if !read {
				return nil, err
			}
			return entries, nil
		}
		entries, _, _ = v.lib.snapshot()
	}
	return entries, nil
}

// lookup finds what was said among the kinds given, reading the library again once if it is not there.
func (v *Voice) lookup(ctx context.Context, token, title string, kinds ...string) (*Match, string) {
	entries, err := v.entries(ctx, token, false)
	if err != nil {
		slog.Debug("plex: no library to look in", "err", err)
		return nil, sayNoServer
	}
	m := find(entries, title, kinds...)
	if m == nil {
		if entries, err = v.entries(ctx, token, true); err == nil {
			m = find(entries, title, kinds...)
		}
	}
	if m == nil {
		return nil, fmt.Sprintf("I couldn't find %s in your Plex library.", strings.TrimSpace(title))
	}
	return m, ""
}

// onServer runs fn with srv as a Source. A failure that is not the device's may be the transient token
// gone stale (the server restarted): fn runs once more with a new one.
func (v *Voice) onServer(ctx context.Context, srv *server, fn func(Source) error) error {
	for try := 0; ; try++ {
		src, err := v.source(ctx, srv)
		if err != nil {
			slog.Debug("plex: no token to play with", "err", err)
			return errUnreachable
		}
		err = fn(src)
		var me *MediaError
		if err == nil || try > 0 || errors.As(err, &me) || ctx.Err() != nil {
			return err
		}
		v.dropTransient(srv)
	}
}

var errUnreachable = errors.New("the server is not reachable")

// answerFor is what is said for an error of a command that plays.
func answerFor(err error) string {
	if errors.Is(err, errUnreachable) {
		return sayNoServer
	}
	return mediaProblem(err)
}

func (v *Voice) play(ctx context.Context, title string, over bool) string {
	token := v.Token()
	if token == "" {
		return sayNotLinked
	}
	if strings.TrimSpace(title) == "" {
		return sayWhat
	}
	m, why := v.lookup(ctx, token, title)
	if m == nil {
		return why
	}
	e := m.Entry
	var answer string
	empty := false
	err := v.onServer(ctx, e.server, func(src Source) error {
		var it companion.Item
		if e.Kind == KindShow {
			eps, err := v.Player.Episodes(ctx, src, e.RatingKey)
			if err != nil {
				return err
			}
			ep, ok := nextUp(eps)
			if !ok {
				empty = true
				return nil
			}
			it = ep
		} else {
			var err error
			if it, err = v.Player.Describe(ctx, src, "/library/metadata/"+e.RatingKey); err != nil {
				return err
			}
		}
		var err error
		answer, err = v.begin(ctx, src, e.server, it, name(m), over)
		return err
	})
	switch {
	case err != nil:
		return answerFor(err)
	case empty:
		return fmt.Sprintf("%s has no episodes in your Plex library.", name(m))
	}
	return answer
}

// begin plays it from its beginning, or, left part way with the beginning not asked for, asks which.
func (v *Voice) begin(ctx context.Context, src Source, srv *server, it companion.Item, name string, over bool) (string, error) {
	if !over && it.ViewOffset > 0 {
		v.ask(&choice{it: it, name: name, srv: srv, at: it.ViewOffset})
		return question(it, name), nil
	}
	v.answered() // a question before this one is moot
	played, err := v.local.Play(ctx, src, it.Key, 0)
	if err != nil {
		return "", err
	}
	v.from, v.last = srv, &lastPlay{it: played, name: name, srv: srv}
	return playing(played, name), nil
}

// question asks whether to resume: "Project Hail Mary is 1 hour 10 minutes in. Resume, or start from
// the beginning?" Ending in a question mark, it has Home Assistant listen for the answer.
func question(it companion.Item, name string) string {
	return fmt.Sprintf("%s is %s in. Resume, or start from the beginning?", what(it, name), spoken(it.ViewOffset))
}

// what is how an answer names an item: the film, or the show with the episode's numbers.
func what(it companion.Item, name string) string {
	if it.Type == "episode" {
		return fmt.Sprintf("%s, season %d episode %d", name, it.Season, it.Episode)
	}
	return name
}

// choose answers the question waiting: to resume, or to start over.
func (v *Voice) choose(ctx context.Context, answer string) string {
	resume, ok := choiceOf(answer)
	v.pmu.Lock()
	c := v.pending
	v.pmu.Unlock()
	if c == nil {
		return "There's nothing to choose."
	}
	if !ok {
		return "Resume, or start from the beginning?"
	}
	v.answered()
	at := time.Duration(0)
	if resume {
		at = c.at
	}
	var it companion.Item
	err := v.onServer(ctx, c.srv, func(src Source) error {
		var err error
		it, err = v.local.Play(ctx, src, c.it.Key, at)
		return err
	})
	if err != nil {
		return answerFor(err)
	}
	v.from, v.last = c.srv, &lastPlay{it: it, name: c.name, srv: c.srv}
	return playing(it, c.name)
}

// filler is what may come with an answer and means nothing.
var filler = map[string]bool{"and": true, "um": true, "uh": true, "er": true, "please": true, "the": true,
	"oh": true, "ok": true, "okay": true, "just": true, "it": true, "from": true, "at": true}

// choiceOf reads an answer: to resume (true) or to start over (false), and whether it said either.
func choiceOf(answer string) (resume, ok bool) {
	var words []string
	for _, w := range strings.Fields(normalize(answer)) {
		if !filler[w] {
			words = append(words, w)
		}
	}
	text := " " + strings.Join(words, " ") + " "
	for _, w := range []string{" beginning ", " start ", " restart ", " over ", " no ", " nope ", " top "} {
		if strings.Contains(text, w) {
			return false, true
		}
	}
	for _, w := range []string{" resume ", " continue ", " going ", " left off ", " yes ", " yeah ", " yep ", " sure "} {
		if strings.Contains(text, w) {
			return true, true
		}
	}
	return false, false
}

func (v *Voice) playEpisode(ctx context.Context, title string, season, episode int, over bool) string {
	token := v.Token()
	if token == "" {
		return sayNotLinked
	}
	if strings.TrimSpace(title) == "" {
		return "Which show?"
	}
	m, why := v.lookup(ctx, token, title, KindShow)
	if m == nil {
		return why
	}
	var answer string
	found := false
	err := v.onServer(ctx, m.Entry.server, func(src Source) error {
		eps, err := v.Player.Episodes(ctx, src, m.Entry.RatingKey)
		if err != nil {
			return err
		}
		// Some libraries number a season's episodes with the season in front (season 2's third is
		// 203): an episode said plainly is looked for that way too, when the plain number is not there.
		for _, want := range []int{episode, season*100 + episode} {
			if want != episode && (episode >= 100 || season < 1) {
				break
			}
			for _, ep := range eps {
				if ep.Season == season && ep.Episode == want {
					found = true
					answer, err = v.begin(ctx, src, m.Entry.server, ep, name(m), over)
					return err
				}
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return answerFor(err)
	case !found:
		return fmt.Sprintf("%s has no season %d episode %d in your Plex library.", name(m), season, episode)
	}
	return answer
}

func (v *Voice) nextEpisode(ctx context.Context) string {
	cur, st := v.local.Current()
	if st == "" || v.from == nil {
		return sayNothing
	}
	if cur.Type != "episode" || cur.ShowKey == "" {
		return "What's playing isn't an episode."
	}
	var it companion.Item
	last, found := false, false
	err := v.onServer(ctx, v.from, func(src Source) error {
		eps, err := v.Player.Episodes(ctx, src, cur.ShowKey)
		if err != nil {
			return err
		}
		for i, ep := range eps {
			if ep.RatingKey != cur.RatingKey {
				continue
			}
			found = true
			if i+1 >= len(eps) {
				last = true
				return nil
			}
			it, err = v.local.Play(ctx, src, eps[i+1].Key, 0)
			return err
		}
		return nil
	})
	switch {
	case err != nil:
		return answerFor(err)
	case last:
		return fmt.Sprintf("That was the last episode of %s.", cur.Show)
	case !found:
		return "I couldn't find the next episode."
	}
	v.last = &lastPlay{it: it, name: cur.Show, srv: v.from}
	return playing(it, cur.Show)
}

// lastPlay is what voice played last: the item, what the answer called it, and its server.
type lastPlay struct {
	it   companion.Item
	name string
	srv  *server
}

func startAt(over bool) time.Duration {
	if over {
		return 0
	}
	return companion.Resume
}

// restart plays what voice played last, playing or not, from its beginning.
func (v *Voice) restart(ctx context.Context) string {
	last := v.last
	if last == nil {
		return "There's nothing to start over."
	}
	var it companion.Item
	err := v.onServer(ctx, last.srv, func(src Source) error {
		var err error
		it, err = v.local.Play(ctx, src, last.it.Key, 0)
		return err
	})
	if err != nil {
		return answerFor(err)
	}
	v.answered()
	v.from, v.last = last.srv, &lastPlay{it: it, name: last.name, srv: last.srv}
	if it.Type == "episode" {
		return fmt.Sprintf("Starting %s, season %d episode %d from the beginning.", last.name, it.Season, it.Episode)
	}
	return fmt.Sprintf("Starting %s from the beginning.", last.name)
}

// spokenEpisode is a title with an episode's numbers after it: "the office season 2 episode 4".
var spokenEpisode = regexp.MustCompile(`(?i)^(.*?)\s+season\s+(\S+?),?\s+episode\s+(.+?)$`)

// overWords are what a title may end in to ask for the beginning: "project hail mary from the start".
var overWords = []string{"from the beginning", "from the start", "from beginning", "from start"}

// flag is a bool argument as Home Assistant may send it: true, "true", "yes", "on" or 1.
func flag(args map[string]any, name string) bool {
	switch v := args[name].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "1", "on":
			return true
		}
	case float64:
		return v != 0
	}
	return false
}

// fromStart is the title without a trailing "from the beginning", and whether that, or the from_start
// argument, asks for the beginning.
func fromStart(title string, args map[string]any) (string, bool) {
	over := flag(args, "from_start")
	t := strings.TrimSpace(title)
	low := strings.ToLower(t)
	for _, w := range overWords {
		if strings.HasSuffix(low, " "+w) && len(t) > len(w)+1 {
			return strings.TrimSpace(t[:len(t)-len(w)-1]), true
		}
	}
	return t, over
}

// nextUp is the episode a show goes on with: one started and not finished; else the first not watched
// after the last one watched; else the first not watched; else, every one watched, the first. Specials
// (season 0) only when the show has nothing else.
func nextUp(all []companion.Item) (companion.Item, bool) {
	var eps []companion.Item
	for _, e := range all {
		if e.Season > 0 {
			eps = append(eps, e)
		}
	}
	if len(eps) == 0 {
		eps = all
	}
	if len(eps) == 0 {
		return companion.Item{}, false
	}
	for _, e := range eps {
		if !e.Watched && e.ViewOffset > 0 {
			return e, true
		}
	}
	last := -1
	for i, e := range eps {
		if e.Watched {
			last = i
		}
	}
	for _, e := range eps[last+1:] {
		if !e.Watched {
			return e, true
		}
	}
	for _, e := range eps {
		if !e.Watched {
			return e, true
		}
	}
	return eps[0], true
}

// name is how the answer calls the match: with its year when another of the same title has another,
// or as the show or the film when one of each has the title.
func name(m *Match) string {
	e := m.Entry
	for _, o := range m.Ties {
		if o.Year != e.Year && e.Year != 0 {
			return fmt.Sprintf("%s (%d)", e.Title, e.Year)
		}
	}
	for _, o := range m.Ties {
		if o.Kind != e.Kind {
			if e.Kind == KindShow {
				return e.Title + ", the show"
			}
			return e.Title + ", the movie"
		}
	}
	return e.Title
}

// playing is the answer for an item started: "Playing The Office, season 2 episode 4.", or, for one
// taken up where it was left, "Resuming Project Hail Mary at 1 hour 10 minutes."
func playing(it companion.Item, title string) string {
	if it.ViewOffset > 0 {
		return fmt.Sprintf("Resuming %s at %s.", what(it, title), spoken(it.ViewOffset))
	}
	return "Playing " + what(it, title) + "."
}

// spoken is a place in an item as it is said: "1 hour 2 minutes", "45 minutes", "30 seconds".
func spoken(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h > 0 && m > 0:
		return plural(h, "hour") + " " + plural(m, "minute")
	case h > 0:
		return plural(h, "hour")
	case m > 0:
		return plural(m, "minute")
	}
	return plural(max(int(d/time.Second), 1), "second")
}

// mediaProblem is what is said when the device would not play, pause or resume.
func mediaProblem(err error) string {
	var pe *MediaError
	if errors.As(err, &pe) {
		return pe.Say
	}
	slog.Debug("plex: voice could not play", "err", err)
	return "Plex couldn't play that on this device."
}

// MediaError is the device refusing a media op, with what to say about it.
type MediaError struct {
	Say string
	Err error
}

func (e *MediaError) Error() string { return e.Err.Error() }
func (e *MediaError) Unwrap() error { return e.Err }

func str(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// number is a season's or an episode's number, as digits or as a word: "4", "four", "4th".
func number(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, suffix := range []string{"st", "nd", "rd", "th"} {
		if t := strings.TrimSuffix(s, suffix); t != s {
			if _, err := strconv.Atoi(t); err == nil {
				s = t
			}
		}
	}
	if d, ok := numbers[s]; ok {
		s = d
	} else if d := normalize(s); strings.IndexFunc(s, unicode.IsLetter) >= 0 && d != "" && !strings.Contains(d, " ") {
		s = d // "twenty one", "twenty-one": 21
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 9999 {
		return 0, false
	}
	return n, true
}
