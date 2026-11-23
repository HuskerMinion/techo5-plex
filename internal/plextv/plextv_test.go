package plextv

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/state"
)

// fakePlex is plex.tv as far as the link and the device list go.
type fakePlex struct {
	mu      sync.Mutex
	polls   int
	put     string
	deleted bool
	queries []string
}

func (f *fakePlex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, r.URL.RawQuery)
	if r.Header.Get("X-Plex-Client-Identifier") != "client-1" || r.Header.Get("X-Plex-Product") != Product {
		http.Error(w, "who are you", 400)
		return
	}
	authed := r.Header.Get("X-Plex-Token") == "account-token"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/pins":
		io.WriteString(w, `{"id":4711,"code":"AB12","expiresIn":900}`)
	case r.URL.Path == "/api/v2/pins/4711":
		f.polls++
		if f.polls < 2 {
			io.WriteString(w, `{"id":4711,"code":"AB12"}`)
			return
		}
		io.WriteString(w, `{"id":"4711","code":"AB12","authToken":"account-token"}`)
	case r.URL.Path == "/api/v2/user" && authed:
		io.WriteString(w, `{"id":99,"username":"kim"}`)
	case r.URL.Path == "/devices.xml" && authed:
		io.WriteString(w, `<MediaContainer><Device id="555" clientIdentifier="client-1"/><Device id="6" clientIdentifier="other"/></MediaContainer>`)
	case r.Method == http.MethodPut && r.URL.Path == "/devices/555" && authed:
		f.put = r.URL.Query().Get("Connection[][uri]")
	case r.Method == http.MethodDelete && r.URL.Path == "/devices/555.xml" && authed:
		f.deleted = true
	default:
		http.Error(w, "no", 404)
	}
}

// The whole link: a code, the poll, the token stored, the account told where the player is, and an
// unlink that forgets it. The token travels as a header only.
func TestLinkAndUnlink(t *testing.T) {
	fp := &fakePlex{}
	srv := httptest.NewServer(fp)
	defer srv.Close()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Update(func(s *state.State) { s.ClientID = "client-1" })
	c := &Client{ID: NewIdentity("client-1", "Kitchen", "0.1.0", ""), API: srv.URL + "/api/v2", Devices: srv.URL, HTTP: srv.Client()}
	l := NewLinker(c, store, func() []string { return []string{"http://192.168.1.9:32500"} })
	l.Poll = 10 * time.Millisecond

	code, err := l.Start()
	if err != nil || code != "AB12" {
		t.Fatalf("code %q %v", code, err)
	}
	if st := l.Status(); !st.Waiting || st.Code != "AB12" {
		t.Fatalf("status while waiting %+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !l.Status().Linked {
		if time.Now().After(deadline) {
			t.Fatal("never linked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := l.Status(); st.User != "kim" || st.Code != "" {
		t.Errorf("linked status %+v", st)
	}
	for time.Now().Before(deadline) {
		fp.mu.Lock()
		put := fp.put
		fp.mu.Unlock()
		if put != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fp.mu.Lock()
	if fp.put != "http://192.168.1.9:32500" {
		t.Errorf("published %q", fp.put)
	}
	fp.mu.Unlock()

	if err := l.Unlink(); err != nil {
		t.Fatal(err)
	}
	if store.Get().Linked() || store.Get().ClientID != "client-1" {
		t.Errorf("after unlink %+v", store.Get())
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if !fp.deleted {
		t.Error("the device was not taken out of the account")
	}
	for _, q := range fp.queries {
		if strings.Contains(q, "account-token") {
			t.Errorf("the token was in a query: %q", q)
		}
	}
}

// A code that runs out says so, in words for the page.
func TestAnExpiredCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			io.WriteString(w, `{"id":1,"code":"ZZ99"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	store, _ := state.Open(t.TempDir())
	c := &Client{ID: NewIdentity("x", "K", "", ""), API: srv.URL, Devices: srv.URL, HTTP: srv.Client()}
	l := NewLinker(c, store, nil)
	l.Poll = 5 * time.Millisecond
	if _, err := l.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for l.Status().Waiting {
		if time.Now().After(deadline) {
			t.Fatal("still waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := l.Status(); !strings.Contains(st.Err, "expired") || st.Linked {
		t.Errorf("status %+v", st)
	}
}

// The account's servers are read from their local connections, plex.direct names included; players,
// relays and remote addresses are left out.
func TestLocalServers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/resources" || r.URL.Query().Get("includeHttps") != "1" ||
			r.Header.Get("X-Plex-Token") != "account-token" || r.Header.Get("Accept") != "application/json" {
			http.Error(w, "no", 400)
			return
		}
		io.WriteString(w, `[
			{"name":"Basement","provides":"server","connections":[
				{"protocol":"https","address":"192.168.1.10","port":32400,"uri":"https://192-168-1-10.abc.plex.direct:32400","local":true,"relay":false},
				{"protocol":"https","address":"203.0.113.7","port":32400,"uri":"https://203-0-113-7.abc.plex.direct:32400","local":false,"relay":false},
				{"protocol":"https","address":"192.168.1.99","port":8443,"uri":"https://192-168-1-99.abc.plex.direct:8443","local":true,"relay":true}]},
			{"name":"Attic","provides":"server,player","connections":[
				{"protocol":"https","address":"192-168-1-11.def.plex.direct","port":32400,"uri":"https://192-168-1-11.def.plex.direct:32400","local":true}]},
			{"name":"Phone","provides":"player,pubsub-player","connections":[
				{"protocol":"http","address":"192.168.2.50","port":32500,"uri":"http://192.168.2.50:32500","local":true}]}
		]`)
	}))
	defer srv.Close()
	c := NewClient(NewIdentity("client-1", "Kitchen", "0.1.0", ""))
	c.Resources = srv.URL + "/api/v2"
	ips, err := c.LocalServers(context.Background(), "account-token")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(ips); got != "[192.168.1.10 192.168.1.11]" {
		t.Errorf("servers %s", got)
	}
}

// ServerAccess finds the account's own token for one server, and the hosts it may be sent to.
func TestServerAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "account-token" {
			http.Error(w, "no", 401)
			return
		}
		io.WriteString(w, `[
			{"clientIdentifier":"other","provides":"server","accessToken":"other-token","connections":[{"address":"192.168.1.20","uri":"https://192-168-1-20.xyz.plex.direct:32400"}]},
			{"clientIdentifier":"phone","provides":"player","accessToken":"phone-token","connections":[]},
			{"clientIdentifier":"basement","provides":"server","accessToken":"server-token","connections":[
				{"address":"192.168.1.10","uri":"https://192-168-1-10.ABC.plex.direct:32400"}]}
		]`)
	}))
	defer srv.Close()
	c := NewClient(NewIdentity("client-1", "Kitchen", "0.1.0", ""))
	c.Resources = srv.URL + "/api/v2"
	access, hosts, err := c.ServerAccess(context.Background(), "account-token", "basement")
	if err != nil || access != "server-token" {
		t.Fatalf("access %q err %v", access, err)
	}
	if got := fmt.Sprint(hosts); got != "[192.168.1.10 192-168-1-10.abc.plex.direct]" {
		t.Errorf("hosts %s", got)
	}
	if _, _, err := c.ServerAccess(context.Background(), "account-token", "phone"); err == nil {
		t.Error("a player was taken for a server")
	}
}
