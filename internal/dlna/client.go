// Package dlna drives the device's own UPnP MediaRenderer: TECHO5's DLNA renderer, which plays music
// through the speaker (with Now Playing) and video full screen. The add-on decodes nothing itself; it
// hands the renderer a URL and DIDL-Lite metadata and asks it what it is doing.
//
// The URLs handed over can carry a Plex token, so nothing here ever puts a URL it was given into an
// error or a log line.
package dlna

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	AVTransport      = "urn:schemas-upnp-org:service:AVTransport:1"
	RenderingControl = "urn:schemas-upnp-org:service:RenderingControl:1"
)

// DefaultDescription is TECHO5's renderer on the device itself: the web port (8181) under /dlna/.
const DefaultDescription = "http://127.0.0.1:8181/dlna/device.xml"

// Renderer is one MediaRenderer's control URLs.
type Renderer struct {
	Name string // its friendlyName
	AVT  string // AVTransport control URL
	RC   string // RenderingControl control URL
	// AVTEvents is AVTransport's event subscription URL, "" when the description names none.
	AVTEvents string
	HTTP      *http.Client
}

// mostSOAP bounds an answer.
const mostSOAP = 256 << 10

// Discover reads a renderer's description and finds its two services' control URLs. A description
// that cannot be read is an error; one without RenderingControl leaves RC empty (volume then does
// nothing).
func Discover(ctx context.Context, hc *http.Client, description string) (*Renderer, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	base, err := url.Parse(description)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, description, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the renderer's description answered %s (is DLNA on?)", resp.Status)
	}
	var d struct {
		URLBase string `xml:"URLBase"`
		Device  struct {
			FriendlyName string     `xml:"friendlyName"`
			Services     []svcXML   `xml:"serviceList>service"`
			Devices      []embedded `xml:"deviceList>device"`
		} `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, mostSOAP)).Decode(&d); err != nil {
		return nil, fmt.Errorf("the renderer's description was not readable: %w", err)
	}
	if d.URLBase != "" {
		if b, err := url.Parse(d.URLBase); err == nil {
			base = b
		}
	}
	services := d.Device.Services
	for _, e := range d.Device.Devices {
		services = append(services, e.Services...)
	}
	r := &Renderer{Name: d.Device.FriendlyName, HTTP: hc}
	for _, s := range services {
		u, err := base.Parse(strings.TrimSpace(s.ControlURL))
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:AVTransport:"):
			r.AVT = u.String()
			if ev := strings.TrimSpace(s.EventURL); ev != "" {
				if e, err := base.Parse(ev); err == nil {
					r.AVTEvents = e.String()
				}
			}
		case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:RenderingControl:"):
			r.RC = u.String()
		}
	}
	if r.AVT == "" {
		return nil, errors.New("the renderer has no AVTransport service")
	}
	return r, nil
}

type svcXML struct {
	Type       string `xml:"serviceType"`
	ControlURL string `xml:"controlURL"`
	EventURL   string `xml:"eventSubURL"`
}

type embedded struct {
	Services []svcXML `xml:"serviceList>service"`
}

// Fault is a UPnP error the renderer answered with.
type Fault struct {
	Code int
	Desc string
}

func (f Fault) Error() string { return fmt.Sprintf("the renderer refused: %d %s", f.Code, f.Desc) }

// Fault codes worth telling apart.
const (
	FaultTransition  = 701 // Transition not available
	FaultSeekMode    = 710 // Seek mode not supported
	FaultIllegalMIME = 714 // Illegal MIME-type (video off)
)

// IsFault is whether err is a fault with one of the codes.
func IsFault(err error, codes ...int) bool {
	var f Fault
	if !errors.As(err, &f) {
		return false
	}
	for _, c := range codes {
		if f.Code == c {
			return true
		}
	}
	return len(codes) == 0
}

// call runs one action and answers its out arguments by name.
func (r *Renderer) call(ctx context.Context, control, service, action string, args [][2]string) (map[string]string, error) {
	if control == "" {
		return nil, errors.New("the renderer has no such service")
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	b.WriteString(`<u:` + action + ` xmlns:u="` + service + `">`)
	for _, a := range args {
		b.WriteString("<" + a[0] + ">" + Escape(a[1]) + "</" + a[0] + ">")
	}
	b.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, control, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+service+"#"+action+`"`)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, mostSOAP))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if resp.StatusCode != http.StatusOK {
		if f, ok := parseFault(body); ok {
			return nil, f
		}
		return nil, fmt.Errorf("%s: the renderer answered %s", action, resp.Status)
	}
	return parseOut(body), nil
}

// parseOut reads the response element's children by local name.
func parseOut(body []byte) map[string]string {
	out := map[string]string{}
	d := xml.NewDecoder(bytes.NewReader(body))
	depth, inResp := 0, false
	var cur string
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if !inResp && strings.HasSuffix(t.Name.Local, "Response") {
				inResp = true
				continue
			}
			if inResp && cur == "" {
				cur = t.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if cur != "" {
				text.Write(t)
			}
		case xml.EndElement:
			depth--
			if cur != "" && t.Name.Local == cur {
				out[cur] = text.String()
				cur = ""
			}
		}
	}
}

func parseFault(body []byte) (Fault, bool) {
	var f struct {
		Code int    `xml:"Body>Fault>detail>UPnPError>errorCode"`
		Desc string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
	}
	if err := xml.Unmarshal(body, &f); err != nil || f.Code == 0 {
		return Fault{}, false
	}
	return Fault{f.Code, f.Desc}, true
}

var instance = [2]string{"InstanceID", "0"}

// SetURI hands the renderer a URL and its description. TECHO5's renderer starts it at once if
// something is playing; Play starts it otherwise.
func (r *Renderer) SetURI(ctx context.Context, uri, meta string) error {
	_, err := r.call(ctx, r.AVT, AVTransport, "SetAVTransportURI",
		[][2]string{instance, {"CurrentURI", uri}, {"CurrentURIMetaData", meta}})
	return err
}

func (r *Renderer) Play(ctx context.Context) error {
	_, err := r.call(ctx, r.AVT, AVTransport, "Play", [][2]string{instance, {"Speed", "1"}})
	return err
}

func (r *Renderer) Pause(ctx context.Context) error {
	_, err := r.call(ctx, r.AVT, AVTransport, "Pause", [][2]string{instance})
	return err
}

func (r *Renderer) Stop(ctx context.Context) error {
	_, err := r.call(ctx, r.AVT, AVTransport, "Stop", [][2]string{instance})
	return err
}

// Seek moves to pos from the start of the URL that is playing (REL_TIME).
func (r *Renderer) Seek(ctx context.Context, pos time.Duration) error {
	_, err := r.call(ctx, r.AVT, AVTransport, "Seek", [][2]string{instance, {"Unit", "REL_TIME"}, {"Target", HMS(pos)}})
	return err
}

// Transport states, as UPnP names them.
const (
	Playing       = "PLAYING"
	Paused        = "PAUSED_PLAYBACK"
	Stopped       = "STOPPED"
	Transitioning = "TRANSITIONING"
	NoMedia       = "NO_MEDIA_PRESENT"
)

// TransportState is the renderer's state.
func (r *Renderer) TransportState(ctx context.Context) (string, error) {
	out, err := r.call(ctx, r.AVT, AVTransport, "GetTransportInfo", [][2]string{instance})
	if err != nil {
		return "", err
	}
	return out["CurrentTransportState"], nil
}

// Position is GetPositionInfo's answer. URI may carry a token: compare it, never log it.
type Position struct {
	URI      string
	Rel, Dur time.Duration
}

func (r *Renderer) Position(ctx context.Context) (Position, error) {
	out, err := r.call(ctx, r.AVT, AVTransport, "GetPositionInfo", [][2]string{instance})
	if err != nil {
		return Position{}, err
	}
	return Position{URI: out["TrackURI"], Rel: ParseHMS(out["RelTime"]), Dur: ParseHMS(out["TrackDuration"])}, nil
}

// SetVolume is 0 to 100.
func (r *Renderer) SetVolume(ctx context.Context, v int) error {
	v = min(max(v, 0), 100)
	_, err := r.call(ctx, r.RC, RenderingControl, "SetVolume",
		[][2]string{instance, {"Channel", "Master"}, {"DesiredVolume", strconv.Itoa(v)}})
	return err
}

// Volume is 0 to 100.
func (r *Renderer) Volume(ctx context.Context) (int, error) {
	out, err := r.call(ctx, r.RC, RenderingControl, "GetVolume", [][2]string{instance, {"Channel", "Master"}})
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(out["CurrentVolume"]))
	if err != nil {
		return 0, errors.New("the renderer's volume was not a number")
	}
	return v, nil
}

// HMS is a time as UPnP writes it: H:MM:SS.
func HMS(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d / time.Second)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

// ParseHMS reads H:MM:SS with or without a fraction; anything else is 0.
func ParseHMS(v string) time.Duration {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 3 {
		return 0
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil || h < 0 || m < 0 || sec < 0 {
		return 0
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec*float64(time.Second))
}

// Escape is text for an XML element or attribute.
func Escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
