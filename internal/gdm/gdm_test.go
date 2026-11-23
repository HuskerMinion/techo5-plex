package gdm

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

var info = Info{Name: "Kitchen", ClientID: "abc-123", Product: "TECHO5", Version: "0.1.0", DeviceClass: "tv",
	Port: 32500, ProtocolVersion: 3, Capabilities: "timeline,playback,playqueues"}

// A search is one line, "M-SEARCH * " and a version, then whatever blank lines the sender sends.
func TestWhichDatagramsAreSearches(t *testing.T) {
	for _, s := range []string{"M-SEARCH * HTTP/1.0\r\n\r\n", "M-SEARCH * HTTP/1.1\r\n\r\n", "M-SEARCH * HTTP/1.0\n\n", "M-SEARCH * HTTP/1.0"} {
		if !IsSearch([]byte(s)) {
			t.Errorf("%q is not read as a search", s)
		}
	}
	for _, s := range []string{"", "NOTIFY * HTTP/1.0\r\n\r\n", "M-SEARCH : HTTP/1.0\r\n", "M-SEARCH * ", "garbage"} {
		if IsSearch([]byte(s)) {
			t.Errorf("%q is read as a search", s)
		}
	}
}

func headerMap(t *testing.T, packet, first string) map[string]string {
	t.Helper()
	if !strings.HasPrefix(packet, first+"\r\n") || !strings.HasSuffix(packet, "\r\n\r\n") {
		t.Fatalf("framing: %q", packet)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(packet, first+"\r\n"), "\r\n\r\n"), "\r\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("not a header: %q", line)
		}
		out[k] = v
	}
	return out
}

// The answer names the player, its control port and its identifier, as Plex's own players do.
func TestTheAnswer(t *testing.T) {
	h := headerMap(t, Answer(info), "HTTP/1.0 200 OK")
	want := map[string]string{"Content-Type": "plex/media-player", "Name": "Kitchen", "Port": "32500", "Product": "TECHO5",
		"Protocol": "plex", "Protocol-Capabilities": "timeline,playback,playqueues", "Protocol-Version": "3",
		"Resource-Identifier": "abc-123", "Version": "0.1.0", "Device-Class": "tv"}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("%s: %q, want %q", k, h[k], v)
		}
	}
	headerMap(t, HelloPacket(info), "HELLO * HTTP/1.0")
	headerMap(t, ByePacket(info), "BYE * HTTP/1.0")
}

// A name from the owner cannot become a second header.
func TestANameIsOneHeader(t *testing.T) {
	i := info
	i.Name = "Living\r\nPort: 1"
	h := headerMap(t, Answer(i), "HTTP/1.0 200 OK")
	if h["Port"] != "32500" || h["Name"] != "LivingPort: 1" {
		t.Errorf("headers %v", h)
	}
}

// A search sent to the server is answered, to the port it came from.
func TestASearchIsAnswered(t *testing.T) {
	addr := make(chan net.Addr, 1)
	s := &Server{Info: func() Info { return info }, Listen: "127.0.0.1:0", OnListen: func(a net.Addr) { addr <- a }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	var to net.Addr
	select {
	case to = <-addr:
	case <-time.After(5 * time.Second):
		t.Fatal("not listening")
	}
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo([]byte("NOTIFY * HTTP/1.0\r\n\r\n"), to); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteTo([]byte("M-SEARCH * HTTP/1.0\r\n\r\n"), to); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, from, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if from.String() != to.String() {
		t.Errorf("answered from %v, not the search port %v", from, to)
	}
	if got := string(buf[:n]); got != Answer(info) {
		t.Errorf("answer %q", got)
	}
}

// A server on another subnet hears the HELLO by unicast, again when told to announce, and BYE at the
// end.
func TestUnicastAnnouncements(t *testing.T) {
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := &Server{Info: func() Info { return info }, Listen: "127.0.0.1:0",
		Peers:    func() []net.IP { return []net.IP{net.IPv4(127, 0, 0, 1)} },
		PeerPort: l.LocalAddr().(*net.UDPAddr).Port}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	read := func(want string) {
		t.Helper()
		_ = l.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 2048)
		n, err := l.Read(buf)
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		headerMap(t, string(buf[:n]), want)
	}
	read("HELLO * HTTP/1.0")
	s.Announce()
	read("HELLO * HTTP/1.0")
	cancel()
	<-done
	read("BYE * HTTP/1.0")
}

// Peers keeps private IPv4 servers only, each once, and the ones casts came from for a day.
func TestPeers(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := &Peers{now: func() time.Time { return now }}
	if !p.SetAccount([]net.IP{net.ParseIP("192.168.1.10"), net.ParseIP("8.8.8.8"), net.ParseIP("192.168.1.10"),
		net.ParseIP("127.0.0.1"), net.ParseIP("fd00::1"), net.ParseIP("10.0.0.2")}) {
		t.Error("the first list is a change")
	}
	if p.SetAccount([]net.IP{net.ParseIP("10.0.0.2"), net.ParseIP("192.168.1.10")}) {
		t.Error("the same list again is not a change")
	}
	if p.Saw(net.ParseIP("192.168.1.10")) {
		t.Error("a server the account lists is not new")
	}
	if p.Saw(net.ParseIP("1.1.1.1")) {
		t.Error("a public address was kept")
	}
	if !p.Saw(net.ParseIP("172.16.5.5")) || p.Saw(net.ParseIP("172.16.5.5")) {
		t.Error("a new server is new once")
	}
	if got := fmt.Sprint(p.List()); got != "[10.0.0.2 172.16.5.5 192.168.1.10]" {
		t.Errorf("list %s", got)
	}
	now = now.Add(25 * time.Hour)
	p.SetAccount(nil)
	if got := fmt.Sprint(p.List()); got != "[]" {
		t.Errorf("after a day and an unlink: %s", got)
	}
}

// A player with no name yet (its renderer not found: DLNA off) does not answer: it could not play, and
// would be listed under a name that tells two devices apart no better than none.
func TestAPlayerWithNoNameDoesNotAnswer(t *testing.T) {
	addr := make(chan net.Addr, 1)
	s := &Server{Info: func() Info { return Info{} }, Listen: "127.0.0.1:0", OnListen: func(a net.Addr) { addr <- a }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	var to net.Addr
	select {
	case to = <-addr:
	case <-time.After(5 * time.Second):
		t.Fatal("not listening")
	}
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo([]byte("M-SEARCH * HTTP/1.0\r\n\r\n"), to); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 2048)
	if n, _, err := c.ReadFrom(buf); err == nil {
		t.Errorf("answered %q", buf[:n])
	}
}
