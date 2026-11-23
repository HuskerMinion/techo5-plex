package dlna_test

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/dlna"
	"github.com/HuskerMinion/techo5-plex/internal/dlna/fakerenderer"
)

func discover(t *testing.T) (*fakerenderer.Renderer, *dlna.Renderer) {
	t.Helper()
	f := fakerenderer.New()
	t.Cleanup(f.Close)
	r, err := dlna.Discover(context.Background(), nil, f.Description())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return f, r
}

// The description's relative control URLs are resolved against it, and the name is read.
func TestDiscoverFindsBothServices(t *testing.T) {
	f, r := discover(t)
	if r.AVT != f.Srv.URL+"/dlna/AVTransport/control" || r.RC != f.Srv.URL+"/dlna/RenderingControl/control" {
		t.Fatalf("control URLs: %q %q", r.AVT, r.RC)
	}
	if r.Name != "Kitchen Show" {
		t.Errorf("name %q", r.Name)
	}
}

// A renderer that is not there (DLNA off: TECHO5 answers 404) is an error that says so.
func TestDiscoverWithDLNAOff(t *testing.T) {
	f := fakerenderer.New()
	defer f.Close()
	_, err := dlna.Discover(context.Background(), nil, f.Srv.URL+"/nothing.xml")
	if err == nil || !strings.Contains(err.Error(), "DLNA") {
		t.Fatalf("got %v", err)
	}
}

// The transport actions arrive with their arguments, and the answers are read back.
func TestTransportActions(t *testing.T) {
	f, r := discover(t)
	ctx := context.Background()
	uri := "http://192.168.1.20:32400/library/parts/1/file.flac?X-Plex-Token=secret"
	if err := r.SetURI(ctx, uri, "<DIDL-Lite/>"); err != nil {
		t.Fatal(err)
	}
	if a := f.Last("SetAVTransportURI"); a == nil || a.Args["CurrentURI"] != uri || a.Args["CurrentURIMetaData"] != "<DIDL-Lite/>" || a.Args["InstanceID"] != "0" {
		t.Fatalf("SetAVTransportURI: %+v", a)
	}
	if err := r.Play(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := r.TransportState(ctx); err != nil || st != dlna.Playing {
		t.Fatalf("state %q %v", st, err)
	}
	f.Set(dlna.Playing, "0:01:05")
	pos, err := r.Position(ctx)
	if err != nil || pos.Rel != 65*time.Second || pos.Dur != 3*time.Minute || pos.URI != uri {
		t.Fatalf("position %+v %v", pos, err)
	}
	if err := r.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.TransportState(ctx); st != dlna.Stopped {
		t.Errorf("after stop: %q", st)
	}
}

// Seek on a renderer that cannot (TECHO5 before real seeking) is fault 710, told apart from others;
// one that can is sent REL_TIME.
func TestSeek(t *testing.T) {
	f, r := discover(t)
	err := r.Seek(context.Background(), 90*time.Second)
	if !dlna.IsFault(err, dlna.FaultSeekMode) {
		t.Fatalf("got %v, want fault 710", err)
	}
	if dlna.IsFault(err, dlna.FaultTransition) {
		t.Error("710 read as 701")
	}
	f.SeekFault = 0
	if err := r.Seek(context.Background(), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if a := f.Last("Seek"); a.Args["Unit"] != "REL_TIME" || a.Args["Target"] != "0:01:30" {
		t.Errorf("seek args %+v", a.Args)
	}
}

func TestVolume(t *testing.T) {
	f, r := discover(t)
	if err := r.SetVolume(context.Background(), 140); err != nil {
		t.Fatal(err)
	}
	if a := f.Last("SetVolume"); a.Args["DesiredVolume"] != "100" || a.Args["Channel"] != "Master" {
		t.Errorf("SetVolume %+v", a.Args)
	}
	if v, err := r.Volume(context.Background()); err != nil || v != 100 {
		t.Errorf("volume %d %v", v, err)
	}
}

// A renderer error never repeats the URL handed to it (it can carry a token).
func TestErrorsDoNotCarryTheURI(t *testing.T) {
	f, r := discover(t)
	f.PlayFault = 701
	_ = r.SetURI(context.Background(), "http://192.168.1.20/x?X-Plex-Token=secret", "")
	err := r.Play(context.Background())
	if !dlna.IsFault(err, dlna.FaultTransition) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("got %v", err)
	}
}

func TestHMS(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:00:00": 0, "1:02:03": time.Hour + 2*time.Minute + 3*time.Second, "0:00:01.500": 1500 * time.Millisecond,
		"NOT_IMPLEMENTED": 0, "": 0, "1:-1:00": 0,
	} {
		if got := dlna.ParseHMS(in); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
	if got := dlna.HMS(3723 * time.Second); got != "1:02:03" {
		t.Errorf("HMS: %q", got)
	}
}

type didl struct {
	Item struct {
		Title  string `xml:"http://purl.org/dc/elements/1.1/ title"`
		Artist string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ artist"`
		Album  string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ album"`
		Art    string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ albumArtURI"`
		Class  string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ class"`
		Res    struct {
			Protocol string `xml:"protocolInfo,attr"`
			Duration string `xml:"duration,attr"`
			URI      string `xml:",chardata"`
		} `xml:"res"`
	} `xml:"item"`
}

// The DIDL-Lite is well formed whatever the names hold, and says music or video in the class TECHO5's
// renderer reads first.
func TestDIDL(t *testing.T) {
	m := dlna.Meta{Title: `Rock & Roll <Live> "1971"`, Artist: "Led Zeppelin", Album: "IV", Art: "http://192.168.1.20:32400/photo?a=1&b=2",
		Dur: 4*time.Minute + 2*time.Second, MIME: "audio/flac", URI: "http://192.168.1.20:32400/x.flac?a=1&X-Plex-Token=t"}
	var d didl
	if err := xml.Unmarshal([]byte(dlna.DIDL(m)), &d); err != nil {
		t.Fatalf("not XML: %v", err)
	}
	if d.Item.Title != m.Title || d.Item.Artist != m.Artist || d.Item.Album != m.Album || d.Item.Art != m.Art {
		t.Errorf("names: %+v", d.Item)
	}
	if d.Item.Class != "object.item.audioItem.musicTrack" || d.Item.Res.Protocol != "http-get:*:audio/flac:*" ||
		d.Item.Res.Duration != "0:04:02.000" || d.Item.Res.URI != m.URI {
		t.Errorf("res/class: %+v", d.Item)
	}
	m.Video, m.MIME, m.Artist, m.Album, m.Art, m.Dur = true, "video/mp4", "", "", "", 0
	d = didl{}
	if err := xml.Unmarshal([]byte(dlna.DIDL(m)), &d); err != nil {
		t.Fatal(err)
	}
	if d.Item.Class != "object.item.videoItem" || d.Item.Res.Protocol != "http-get:*:video/mp4:*" || d.Item.Res.Duration != "" {
		t.Errorf("video: %+v", d.Item)
	}
	if strings.Contains(dlna.DIDL(m), "techo5:start") {
		t.Error("a stream from the start says where it starts")
	}
	// A film sent again from a place says where in it the stream begins, in TECHO5's namespace.
	m.Start, m.Dur = 53*time.Minute, 97*time.Minute
	var r struct {
		Res struct {
			Start string `xml:"urn:techo5:dlna start,attr"`
		} `xml:"item>res"`
	}
	if err := xml.Unmarshal([]byte(dlna.DIDL(m)), &r); err != nil {
		t.Fatal(err)
	}
	if r.Res.Start != "0:53:00" {
		t.Errorf("start %q, want 0:53:00", r.Res.Start)
	}
}
