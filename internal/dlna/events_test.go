package dlna_test

import (
	"context"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/dlna"
	"github.com/HuskerMinion/techo5-plex/internal/dlna/fakerenderer"
)

// LastChange is XML escaped inside the propertyset; instance 0's variables are read by name.
func TestParseLastChange(t *testing.T) {
	body := `<?xml version="1.0"?><e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0"><e:property><LastChange>` +
		`&lt;Event xmlns="urn:schemas-upnp-org:metadata-1-0/AVT/"&gt;&lt;InstanceID val="0"&gt;` +
		`&lt;TransportState val="PLAYING"/&gt;&lt;CurrentTrackMetaData val="&amp;lt;DIDL-Lite/&amp;gt;"/&gt;` +
		`&lt;X_TECHO5_Request val="next:3"/&gt;&lt;/InstanceID&gt;` +
		`&lt;InstanceID val="1"&gt;&lt;TransportState val="STOPPED"/&gt;&lt;/InstanceID&gt;&lt;/Event&gt;` +
		`</LastChange></e:property></e:propertyset>`
	vars, err := dlna.ParseLastChange([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if vars["TransportState"] != "PLAYING" || vars[dlna.RequestVar] != "next:3" || vars["CurrentTrackMetaData"] != "<DIDL-Lite/>" {
		t.Errorf("vars %v", vars)
	}
	for in, want := range map[string]time.Duration{"seek:95:7": 95 * time.Second, "seek:0:1": 0, "seek:95": 0, "next:3": 0, "seek:-4:2": 0, "seek:x:2": 0} {
		if got := dlna.RequestPlace(in); got != want {
			t.Errorf("place of %q: %v, want %v", in, got, want)
		}
	}
	for in, want := range map[string]string{"next": "next", "next:3": "next", " Previous:12 ": "previous", "seek:95:7": "seek", "": "", ":4": ""} {
		if got := dlna.RequestWord(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func startEvents(t *testing.T, f *fakerenderer.Renderer, r *dlna.Renderer) (chan dlna.Event, context.CancelFunc, chan struct{}) {
	t.Helper()
	got := make(chan dlna.Event, 16)
	ev := &dlna.Events{URL: func() string { return r.AVTEvents }, On: func(e dlna.Event) { got <- e },
		RenewEvery: 100 * time.Millisecond, Retry: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = ev.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return got, cancel, done
}

func next(t *testing.T, got chan dlna.Event) dlna.Event {
	t.Helper()
	select {
	case e := <-got:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return dlna.Event{}
	}
}

// The renderer's events come through; a request is handed over once, never from the whole state sent
// on subscribing, and never from an event that came late; a renderer that restarted is subscribed to
// again; stopping unsubscribes.
func TestEvents(t *testing.T) {
	f, r := discover(t)
	if r.AVTEvents != f.Srv.URL+"/dlna/AVTransport/event" {
		t.Fatalf("event URL %q", r.AVTEvents)
	}
	f.Initial = [][2]string{{dlna.RequestVar, "next:2"}}
	got, cancel, done := startEvents(t, f, r)

	if e := next(t, got); e.TransportState() != "NO_MEDIA_PRESENT" || e.Request != "" {
		t.Fatalf("first event %+v", e)
	}
	f.Notify([2]string{"TransportState", "PLAYING"}, [2]string{dlna.RequestVar, "next:2"})
	if e := next(t, got); e.TransportState() != "PLAYING" || e.Request != "" {
		t.Errorf("the request from before was handed over: %+v", e)
	}
	f.Notify([2]string{"TransportState", "PLAYING"}, [2]string{dlna.RequestVar, "next:3"})
	if e := next(t, got); e.Request != "next" {
		t.Errorf("next: %+v", e)
	}
	f.Notify([2]string{"TransportState", "PLAYING"}, [2]string{dlna.RequestVar, "previous:4"})
	if e := next(t, got); e.Request != "previous" {
		t.Errorf("previous: %+v", e)
	}
	f.NotifySeq(1, [2]string{dlna.RequestVar, "next:9"})
	f.Notify([2]string{"TransportState", "PAUSED_PLAYBACK"})
	if e := next(t, got); e.Request != "" || e.TransportState() != "PAUSED_PLAYBACK" {
		t.Errorf("a late event was taken: %+v", e)
	}
	// One without a count, after one without the variable, is a press too.
	f.Notify([2]string{dlna.RequestVar, "next"})
	if e := next(t, got); e.Request != "next" {
		t.Errorf("bare next: %+v", e)
	}

	time.Sleep(250 * time.Millisecond) // a renewal or two
	f.Forget()
	if e := next(t, got); e.TransportState() == "" {
		t.Errorf("not subscribed again after a restart: %+v", e)
	}
	if subs, renewals := f.Counts(); subs != 2 || renewals == 0 {
		t.Errorf("%d subscriptions, %d renewals", subs, renewals)
	}
	cancel()
	<-done
	if n := f.Subscribers(); n != 0 {
		t.Errorf("%d subscriptions left after stopping", n)
	}
}
