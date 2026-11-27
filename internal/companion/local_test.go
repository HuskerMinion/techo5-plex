package companion

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMedia is the device's player over the add-on API.
type fakeMedia struct {
	mu    sync.Mutex
	urls  []string
	ops   []string
	title string
}

func (m *fakeMedia) PlayVideo(_ context.Context, url, title string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.urls, m.title = append(m.urls, url), title
	return strconv.Itoa(len(m.urls)), nil
}

func (m *fakeMedia) op(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops = append(m.ops, name)
	return nil
}

func (m *fakeMedia) Pause(context.Context) error  { return m.op("pause") }
func (m *fakeMedia) Resume(context.Context) error { return m.op("resume") }
func (m *fakeMedia) Stop(context.Context) error   { return m.op("stop") }

func (g *rig) source() Source {
	return Source{Base: g.pms.srv.URL, Machine: "server-1", Token: token}
}

// clock is a time that moves when told.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newLocal(t *testing.T, g *rig) (*Local, *fakeMedia, *clock) {
	m := &fakeMedia{}
	l := NewLocal(g.p, m)
	c := &clock{t: time.Now()}
	l.now = c.now
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)
	return l, m, c
}

func lastReport(g *rig) (state, at string) {
	rs := g.pms.saw("/:/timeline")
	if len(rs) == 0 {
		return "", ""
	}
	q := rs[len(rs)-1].URL.Query()
	return q.Get("state"), q.Get("time")
}

func waitReport(t *testing.T, g *rig, state, at string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, tm := lastReport(g)
		if st == state && (at == "" || tm == at) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the last report is %s at %s, want %s at %s", st, tm, state, at)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func at(media, state string, pos time.Duration) MediaState {
	return MediaState{Media: media, State: state, Position: pos, HasPosition: true}
}

// The server's reports follow what the device says: each change at once, a position while it plays
// every ten seconds, and its end where it was, or at the end of the item when it played to it.
func TestLocalFollowsTheDevice(t *testing.T) {
	g := newRig(t)
	l, m, c := newLocal(t, g)
	it, err := l.Play(context.Background(), g.source(), "/library/metadata/3", 0)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "A Film" || it.Duration != 90*time.Minute || m.title != "A Film" {
		t.Fatalf("played %+v as %q", it, m.title)
	}
	waitReport(t, g, "buffering", "0")
	l.State(at("1", "playing", 2*time.Second))
	waitReport(t, g, "playing", "2000")
	n := len(g.pms.saw("/:/timeline"))
	c.add(3 * time.Second)
	l.State(at("1", "playing", 5*time.Second)) // too soon for another
	c.add(10 * time.Second)
	l.State(at("1", "playing", 15*time.Second))
	waitReport(t, g, "playing", "15000")
	if got := len(g.pms.saw("/:/timeline")); got != n+1 {
		t.Fatalf("%d reports for two positions", got-n)
	}
	l.State(at("1", "paused", 16*time.Second))
	waitReport(t, g, "paused", "16000")
	l.State(MediaState{Media: "9", State: "stopped", Reason: "device"}) // not ours
	if _, st := l.Current(); st != "paused" {
		t.Fatalf("state %q", st)
	}
	l.State(MediaState{Media: "1", State: "stopped", Reason: "ended", Position: 89 * time.Minute, HasPosition: true})
	waitReport(t, g, "stopped", "5400000")
	if _, st := l.Current(); st != "" {
		t.Fatalf("still %s", st)
	}
	if err := l.Pause(context.Background()); err != ErrNothingPlaying {
		t.Fatalf("pause after the end: %v", err)
	}
}

// Replaced by the next play: the one before is reported stopped where the device says it was, when its
// end arrives after the next has started; the next carries on.
func TestLocalReplaced(t *testing.T) {
	g := newRig(t)
	l, _, _ := newLocal(t, g)
	if _, err := l.Play(context.Background(), g.source(), "/library/metadata/3", 0); err != nil {
		t.Fatal(err)
	}
	l.State(at("1", "playing", time.Minute))
	if _, err := l.Play(context.Background(), g.source(), "/library/metadata/3", 0); err != nil {
		t.Fatal(err)
	}
	l.State(MediaState{Media: "1", State: "stopped", Reason: "replaced", Position: 61 * time.Second, HasPosition: true})
	waitReport(t, g, "stopped", "61000")
	if _, st := l.Current(); st != "buffering" {
		t.Fatalf("the next is %q", st)
	}
}

// A state that comes before media.play's answer is not lost.
func TestLocalEarlyState(t *testing.T) {
	g := newRig(t)
	l, _, _ := newLocal(t, g)
	l.State(at("1", "playing", 0))
	if _, err := l.Play(context.Background(), g.source(), "/library/metadata/3", 0); err != nil {
		t.Fatal(err)
	}
	if _, st := l.Current(); st != "playing" {
		t.Fatalf("state %q", st)
	}
}

// After the add-on connects again, media.status says what became of it.
func TestLocalResync(t *testing.T) {
	g := newRig(t)
	l, _, _ := newLocal(t, g)
	if _, err := l.Play(context.Background(), g.source(), "/library/metadata/3", 0); err != nil {
		t.Fatal(err)
	}
	l.State(at("1", "playing", 30*time.Second))
	mark := l.Mark()
	l.Resync(MediaState{State: MediaIdle}, mark) // the daemon restarted: it is gone
	waitReport(t, g, "stopped", "30000")
	if _, st := l.Current(); st != "" {
		t.Fatalf("still %s", st)
	}
	// A status asked before a play is no word on that play.
	mark = l.Mark()
	if _, err := l.Play(context.Background(), g.source(), "/library/metadata/3", 0); err != nil {
		t.Fatal(err)
	}
	l.Resync(MediaState{State: MediaIdle}, mark)
	if _, st := l.Current(); st != "buffering" {
		t.Fatalf("a play since the status was ended: %q", st)
	}
}

// A film started part way is asked of the server from there.
func TestLocalResumes(t *testing.T) {
	g := newRig(t)
	l, m, _ := newLocal(t, g)
	it, err := l.Play(context.Background(), g.source(), "/library/metadata/3", 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if it.ViewOffset != 20*time.Minute {
		t.Fatalf("started at %v", it.ViewOffset)
	}
	m.mu.Lock()
	u := m.urls[0]
	m.mu.Unlock()
	if !strings.Contains(u, "offset=1200") {
		t.Fatalf("stream %s", u)
	}
	// The device counts from the stream's start, which is 20 minutes in.
	l.State(at("1", "playing", 10*time.Second))
	waitReport(t, g, "playing", "1210000")
}
