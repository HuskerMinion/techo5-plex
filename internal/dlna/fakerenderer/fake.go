// Package fakerenderer is a stand-in for TECHO5's DLNA renderer, for tests: it describes itself as
// TECHO5's does (/dlna/device.xml, control at /dlna/{service}/control), insists on the same things
// (an address in Host, a SOAPACTION matching the body), and records every action.
package fakerenderer

import (
	"bytes"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Action is one call the renderer was sent.
type Action struct {
	Service, Name string
	Args          map[string]string
}

// Renderer is the stand-in.
type Renderer struct {
	Srv *httptest.Server

	mu      sync.Mutex
	Actions []Action
	State   string
	URI     string
	Meta    string
	RelTime string
	Volume  string
	// SeekFault is the fault Seek answers (710 as TECHO5 before real seeking), 0 to accept.
	SeekFault int
	// PlayFault is the fault Play answers, 0 to accept.
	PlayFault int
	// Initial are variables the first event of a subscription carries besides the state (a request
	// made before, as TECHO5's whole-state events would).
	Initial [][2]string

	subs       map[string]*eventSub
	nextSID    int
	subscribes int    // new subscriptions taken
	requests   string // the last new subscription's X-TECHO5-Requests
	renewals   int
}

type eventSub struct {
	callback string
	seq      int
}

// New starts one.
func New() *Renderer {
	f := &Renderer{State: "NO_MEDIA_PRESENT", RelTime: "0:00:00", Volume: "30", SeekFault: 710}
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Description is its description URL.
func (f *Renderer) Description() string { return f.Srv.URL + "/dlna/device.xml" }

// Close stops it.
func (f *Renderer) Close() { f.Srv.Close() }

// Calls is a copy of the actions so far.
func (f *Renderer) Calls() []Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Action(nil), f.Actions...)
}

// Last is the last action with name, or nil.
func (f *Renderer) Last(name string) *Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.Actions) - 1; i >= 0; i-- {
		if f.Actions[i].Name == name {
			a := f.Actions[i]
			return &a
		}
	}
	return nil
}

// Set changes the renderer's state, as the device would on its own.
func (f *Renderer) Set(state, rel string) {
	f.mu.Lock()
	f.State, f.RelTime = state, rel
	f.mu.Unlock()
}

const desc = `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion>` +
	`<device><deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType><friendlyName>Kitchen Show</friendlyName>` +
	`<serviceList>` +
	`<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType><serviceId>urn:upnp-org:serviceId:AVTransport</serviceId>` +
	`<SCPDURL>/dlna/AVTransport.xml</SCPDURL><controlURL>/dlna/AVTransport/control</controlURL><eventSubURL>/dlna/AVTransport/event</eventSubURL></service>` +
	`<service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType><serviceId>urn:upnp-org:serviceId:RenderingControl</serviceId>` +
	`<SCPDURL>/dlna/RenderingControl.xml</SCPDURL><controlURL>/dlna/RenderingControl/control</controlURL><eventSubURL>/dlna/RenderingControl/event</eventSubURL></service>` +
	`</serviceList></device></root>`

func (f *Renderer) serve(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		http.Error(w, "use the device's address", http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == "/dlna/device.xml":
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		_, _ = io.WriteString(w, desc)
	case r.Method == http.MethodPost && r.URL.Path == "/dlna/AVTransport/control":
		f.control(w, r, "urn:schemas-upnp-org:service:AVTransport:1")
	case r.Method == http.MethodPost && r.URL.Path == "/dlna/RenderingControl/control":
		f.control(w, r, "urn:schemas-upnp-org:service:RenderingControl:1")
	case r.URL.Path == "/dlna/AVTransport/event":
		f.event(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *Renderer) control(w http.ResponseWriter, r *http.Request, service string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	name, args := parse(body)
	h := strings.Trim(r.Header.Get("SOAPACTION"), `"`)
	if h != service+"#"+name {
		fault(w, 401, "Invalid Action")
		return
	}
	f.mu.Lock()
	f.Actions = append(f.Actions, Action{Service: service, Name: name, Args: args})
	var out [][2]string
	code := 0
	switch name {
	case "SetAVTransportURI":
		f.URI, f.Meta = args["CurrentURI"], args["CurrentURIMetaData"]
		if f.State == "NO_MEDIA_PRESENT" {
			f.State = "STOPPED"
		}
	case "Play":
		if f.PlayFault != 0 {
			code = f.PlayFault
		} else {
			f.State = "PLAYING"
		}
	case "Pause":
		f.State = "PAUSED_PLAYBACK"
	case "Stop":
		f.State = "STOPPED"
	case "Seek":
		if f.SeekFault != 0 {
			code = f.SeekFault
		} else {
			f.RelTime = args["Target"]
		}
	case "GetTransportInfo":
		out = [][2]string{{"CurrentTransportState", f.State}, {"CurrentTransportStatus", "OK"}, {"CurrentSpeed", "1"}}
	case "GetPositionInfo":
		out = [][2]string{{"Track", "1"}, {"TrackDuration", "0:03:00"}, {"TrackMetaData", f.Meta}, {"TrackURI", f.URI},
			{"RelTime", f.RelTime}, {"AbsTime", "NOT_IMPLEMENTED"}}
	case "SetVolume":
		f.Volume = args["DesiredVolume"]
	case "GetVolume":
		out = [][2]string{{"CurrentVolume", f.Volume}}
	}
	f.mu.Unlock()
	if code != 0 {
		fault(w, code, "refused")
		return
	}
	var b strings.Builder
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:` + name + `Response xmlns:u="` + service + `">`)
	for _, kv := range out {
		b.WriteString("<" + kv[0] + ">")
		_ = xml.EscapeText(&b, []byte(kv[1]))
		b.WriteString("</" + kv[0] + ">")
	}
	b.WriteString(`</u:` + name + `Response></s:Body></s:Envelope>`)
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	_, _ = io.WriteString(w, b.String())
}

func fault(w http.ResponseWriter, code int, desc string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>`+
		`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>`+
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>`+itoa(code)+`</errorCode>`+
		`<errorDescription>`+desc+`</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// parse reads the action's name and its arguments.
func parse(body []byte) (string, map[string]string) {
	d := xml.NewDecoder(strings.NewReader(string(body)))
	args := map[string]string{}
	name, cur := "", ""
	depth := 0
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return name, args
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch depth {
			case 3:
				name = t.Name.Local
			case 4:
				cur = t.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if cur != "" {
				text.Write(t)
			}
		case xml.EndElement:
			if depth == 4 && cur != "" {
				args[cur] = text.String()
				cur = ""
			}
			depth--
		}
	}
}

// event is SUBSCRIBE (new or a renewal) and UNSUBSCRIBE, as TECHO5's renderer takes them: the callback
// must be the subscriber's own address, and the first event (the whole state, SEQ 0) follows the
// answer.
func (f *Renderer) event(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[string]*eventSub{}
	}
	switch r.Method {
	case "UNSUBSCRIBE":
		delete(f.subs, r.Header.Get("SID"))
		w.WriteHeader(http.StatusOK)
	case "SUBSCRIBE":
		if sid := r.Header.Get("SID"); sid != "" {
			if _, ok := f.subs[sid]; !ok {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			f.renewals++
			w.Header().Set("SID", sid)
			w.Header().Set("TIMEOUT", "Second-1800")
			w.WriteHeader(http.StatusOK)
			return
		}
		cb := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("CALLBACK")), "<"), ">")
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		u, err := http.NewRequest("GET", cb, nil)
		if err != nil || r.Header.Get("NT") != "upnp:event" || u.URL.Hostname() != remote {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		f.nextSID++
		sid := "uuid:fake-" + strconv.Itoa(f.nextSID)
		f.subs[sid] = &eventSub{callback: cb, seq: 1}
		f.subscribes++
		f.requests = r.Header.Get("X-TECHO5-Requests")
		body := lastChange(append([][2]string{{"TransportState", f.State}}, f.Initial...))
		w.Header().Set("SID", sid)
		w.Header().Set("TIMEOUT", "Second-1800")
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(20 * time.Millisecond)
			send(cb, sid, 0, body)
		}()
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// Subscribers is how many subscriptions it holds.
func (f *Renderer) Subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// Counts is how many subscriptions were made, and renewed.
func (f *Renderer) Counts() (subscribes, renewals int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribes, f.renewals
}

// Forget drops every subscription, as a renderer that restarted has, and sets what the next first
// event carries besides the state.
func (f *Renderer) Forget(initial ...[2]string) {
	f.mu.Lock()
	f.subs, f.Initial = nil, initial
	f.mu.Unlock()
}

// Notify sends an event with the variables to every subscriber, and returns once each has answered.
func (f *Renderer) Notify(vars ...[2]string) {
	f.NotifySeq(-1, vars...)
}

// NotifySeq is Notify with SEQ seq instead of the next one (-1), for events out of order.
func (f *Renderer) NotifySeq(seq int, vars ...[2]string) {
	f.mu.Lock()
	type note struct {
		cb, sid string
		seq     int
	}
	var notes []note
	for sid, s := range f.subs {
		n := seq
		if n < 0 {
			n = s.seq
			s.seq++
		}
		notes = append(notes, note{s.callback, sid, n})
	}
	f.mu.Unlock()
	body := lastChange(vars)
	for _, n := range notes {
		send(n.cb, n.sid, n.seq, body)
	}
}

// lastChange is a propertyset with the variables in LastChange, escaped as UPnP has it.
func lastChange(vars [][2]string) string {
	var inner strings.Builder
	inner.WriteString(`<Event xmlns="urn:schemas-upnp-org:metadata-1-0/AVT/"><InstanceID val="0">`)
	for _, v := range vars {
		inner.WriteString("<" + v[0] + ` val="`)
		_ = xml.EscapeText(&inner, []byte(v[1]))
		inner.WriteString(`"/>`)
	}
	inner.WriteString(`</InstanceID></Event>`)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0"><e:property><LastChange>`)
	_ = xml.EscapeText(&b, []byte(inner.String()))
	b.WriteString(`</LastChange></e:property></e:propertyset>`)
	return b.String()
}

func send(callback, sid string, seq int, body string) {
	req, err := http.NewRequest("NOTIFY", callback, bytes.NewBufferString(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("NT", "upnp:event")
	req.Header.Set("NTS", "upnp:propchange")
	req.Header.Set("SID", sid)
	req.Header.Set("SEQ", strconv.Itoa(seq))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// Requests is the X-TECHO5-Requests header of the last new subscription: which of TECHO5's own
// requests its controller said it acts on.
func (f *Renderer) Requests() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}
