package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Nothing a controller names is fetched from the device itself, however it is reached.
func TestTheDeviceItselfIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if resp, err := Client(0).Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("a loopback server was fetched from")
	}
	for _, s := range []string{"127.0.0.1", "::1", "169.254.1.1", "fe80::1", "239.0.0.250", "0.0.0.0"} {
		if !Refused(net.ParseIP(s)) {
			t.Errorf("%s is not refused", s)
		}
	}
	old := OwnAddrs
	OwnAddrs = func() []net.IP { return []net.IP{net.ParseIP("192.168.1.9")} }
	defer func() { OwnAddrs = old }()
	if !Refused(net.ParseIP("192.168.1.9")) || Refused(net.ParseIP("192.168.1.20")) {
		t.Error("the device's own address is not told from another's")
	}
}

func TestHostIsAddress(t *testing.T) {
	for h, want := range map[string]bool{"": true, "192.168.1.9:32500": true, "[fe80::1%wlan0]:32500": true,
		"192.168.1.9": true, "evil.example:32500": false, "localhost:8181": false} {
		if got := HostIsAddress(h); got != want {
			t.Errorf("%q: %v", h, got)
		}
	}
}

func TestHome(t *testing.T) {
	for s, want := range map[string]bool{"192.168.1.5": true, "10.0.0.2": true, "127.0.0.1": true, "8.8.8.8": false, "": false} {
		if got := Home(net.ParseIP(s)); got != want {
			t.Errorf("%q: %v", s, got)
		}
	}
}
