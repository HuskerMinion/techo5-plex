package companion

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/netguard"
)

// What a Plex server says a piece of media is, and the controller's coordinates for it. Adapted from
// kevin909867's plexcast (TECHO5 PR #691): metadata.go and playmedia.go. The difference that matters:
// the token goes to the server as a header, never in a URL that can end up in an error.

// mediaRequest is a controller's playMedia: where the server is and which item. token is the
// transient token Plex hands a controller for exactly this.
type mediaRequest struct {
	protocol, address, port string
	key                     string
	token                   string
	machine                 string
	container               string
	typ                     string
	offset                  time.Duration
}

// server is the Plex server as a base URL.
func (m mediaRequest) server() string {
	return m.protocol + "://" + net.JoinHostPort(m.address, m.port)
}

// rendererServer is the Plex server as a base URL for what the device's own player fetches. The
// device's DNS is the house's, which often refuses plex.direct names (netguard.PlexDirectIP), and its
// player has no way to be told the address, so a plex.direct server is handed over as its address
// over plain HTTP on the home network, which Plex servers take from the LAN unless set to require
// secure connections.
func (m mediaRequest) rendererServer() string {
	if ip := netguard.PlexDirectIP(m.address); ip != nil {
		return "http://" + net.JoinHostPort(ip.String(), m.port)
	}
	return m.server()
}

// check is whether a controller's request may be acted on: the address is a host and nothing else,
// the protocol fetchable, the key a path on the server.
func (m mediaRequest) check() error {
	if m.protocol != "http" && m.protocol != "https" {
		return errors.New("the server's protocol is not http or https")
	}
	if m.address == "" || strings.ContainsAny(m.address, "/?#@\\ \t\r\n[]") {
		return errors.New("the server has no address")
	}
	if port, err := strconv.Atoi(m.port); err != nil || port < 1 || port > 65535 {
		return errors.New("the server has no port")
	}
	if !strings.HasPrefix(m.key, "/") || strings.HasPrefix(m.key, "//") || strings.ContainsAny(m.key, " \t\r\n#") {
		return errors.New("the item has no path")
	}
	if m.machine == "" {
		return errors.New("the server is not named")
	}
	return nil
}

// item is one piece of media, as far as this player needs it.
type item struct {
	ratingKey string
	key       string // /library/metadata/1234
	typ       string // movie, episode, track
	title     string
	artist    string // the artist of a song, or the show of an episode
	album     string
	thumb     string
	dur       time.Duration
	part      part
	video     bool
	queue     string // the play queue the controller sent, /playQueues/12?own=1
}

type part struct {
	id         int
	key        string
	container  string
	videoCodec string
	audioCodec string
	dur        time.Duration
	streams    []stream
}

type stream struct {
	id       int
	kind     int
	codec    string
	language string
	selected bool
}

const (
	streamVideo    = 1
	streamAudio    = 2
	streamSubtitle = 3
)

func (p part) selected(kind int) int {
	for _, s := range p.streams {
		if s.kind == kind && s.selected {
			return s.id
		}
	}
	return 0
}

func (p *part) selectStream(kind, id int) {
	for i := range p.streams {
		if p.streams[i].kind == kind {
			p.streams[i].selected = id != 0 && p.streams[i].id == id
		}
	}
}

func (p part) hasSubtitles() bool {
	for _, s := range p.streams {
		if s.kind == streamSubtitle && s.id != 0 {
			return true
		}
	}
	return false
}

// directMIME is the content type of a file TECHO5's renderer decodes as it is (MP3, FLAC, WAV), or ""
// for anything that must be transcoded first.
func (p part) directMIME() string {
	c := strings.ToLower(p.container)
	a := strings.ToLower(p.audioCodec)
	switch {
	case c == "mp3" || (a == "mp3" && (c == "" || c == "mp3")):
		return "audio/mpeg"
	case c == "flac" || (a == "flac" && (c == "" || c == "flac")):
		return "audio/flac"
	case c == "wav":
		return "audio/wav"
	}
	return ""
}

// The XML the server answers /library/metadata with. Only what this player reads is named.
type containerXML struct {
	// A play queue's own: its version, which controllers compare to know theirs is current, and the
	// item it has selected (playing).
	PlayQueueVersion        string `xml:"playQueueVersion,attr"`
	PlayQueueSelectedItemID string `xml:"playQueueSelectedItemID,attr"`

	Videos []mediaXML `xml:"Video"`
	Tracks []mediaXML `xml:"Track"`
}

type mediaXML struct {
	PlayQueueItemID  string `xml:"playQueueItemID,attr"` // in a play queue only
	RatingKey        string `xml:"ratingKey,attr"`
	Key              string `xml:"key,attr"`
	Type             string `xml:"type,attr"`
	Title            string `xml:"title,attr"`
	GrandparentTitle string `xml:"grandparentTitle,attr"`
	ParentTitle      string `xml:"parentTitle,attr"`
	Thumb            string `xml:"thumb,attr"`
	ParentThumb      string `xml:"parentThumb,attr"`
	Duration         int64  `xml:"duration,attr"`
	Media            []struct {
		Duration   int64  `xml:"duration,attr"`
		Container  string `xml:"container,attr"`
		VideoCodec string `xml:"videoCodec,attr"`
		AudioCodec string `xml:"audioCodec,attr"`
		Parts      []struct {
			ID        int    `xml:"id,attr"`
			Key       string `xml:"key,attr"`
			Container string `xml:"container,attr"`
			Duration  int64  `xml:"duration,attr"`
			Streams   []struct {
				ID         int    `xml:"id,attr"`
				StreamType int    `xml:"streamType,attr"`
				Codec      string `xml:"codec,attr"`
				Language   string `xml:"language,attr"`
				Selected   bool   `xml:"selected,attr"`
			} `xml:"Stream"`
		} `xml:"Part"`
	} `xml:"Media"`
}

const mostMetadata = 4 << 20

// get fetches path on the item's server with the token as a header. Errors say the path and the
// status, never a URL with a token.
func (p *Player) get(ctx context.Context, req mediaRequest, path string, v any) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.server()+path, nil)
	if err != nil {
		return errors.New("the server's address is not usable")
	}
	p.serverHeaders(httpReq, req.token)
	httpReq.Header.Set("Accept", "application/xml")
	resp, err := p.Fetch.Do(httpReq)
	if err != nil {
		return scrub(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, mostMetadata)).Decode(v); err != nil {
		return fmt.Errorf("the server's answer was not readable: %w", err)
	}
	return nil
}

// metadata fetches an item's description.
func (p *Player) metadata(ctx context.Context, req mediaRequest) (*item, error) {
	var c containerXML
	if err := p.get(ctx, req, req.key, &c); err != nil {
		return nil, err
	}
	all := append(append([]mediaXML{}, c.Videos...), c.Tracks...)
	if len(all) == 0 {
		return nil, errors.New("the server described nothing")
	}
	it := fromXML(all[0], len(c.Videos) > 0)
	if it.key == "" {
		it.key = req.key
	}
	it.queue = req.container
	return it, nil
}

func fromXML(m mediaXML, video bool) *item {
	it := &item{
		ratingKey: m.RatingKey,
		key:       m.Key,
		typ:       m.Type,
		title:     m.Title,
		artist:    or(m.GrandparentTitle, m.ParentTitle),
		album:     m.ParentTitle,
		thumb:     or(m.Thumb, m.ParentThumb),
		dur:       time.Duration(m.Duration) * time.Millisecond,
		video:     video,
	}
	if it.artist == it.album {
		it.artist = m.GrandparentTitle
	}
	if video {
		it.album = ""
		if m.Type != "episode" {
			it.artist = ""
		}
	}
	if len(m.Media) > 0 {
		md := m.Media[0]
		it.part.container, it.part.videoCodec, it.part.audioCodec = md.Container, md.VideoCodec, md.AudioCodec
		it.part.dur = time.Duration(md.Duration) * time.Millisecond
		if len(md.Parts) > 0 {
			pt := md.Parts[0]
			it.part.id, it.part.key = pt.ID, pt.Key
			if pt.Container != "" {
				it.part.container = pt.Container
			}
			if pt.Duration > 0 {
				it.part.dur = time.Duration(pt.Duration) * time.Millisecond
			}
			for _, s := range pt.Streams {
				it.part.streams = append(it.part.streams, stream{id: s.ID, kind: s.StreamType, codec: s.Codec, language: s.Language, selected: s.Selected})
			}
		}
	}
	if it.dur == 0 {
		it.dur = it.part.dur
	}
	return it
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
