package companion

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/plextv"
)

// What the renderer is handed. TECHO5's renderer fetches with a plain GET and no headers, so a URL it
// is handed carries the token in its query: the one place a token goes in a URL. Such a URL is kept
// in the session, compared with what the renderer reports, and never written to a log or an error.
//
// Everything else (the decision, the stop, the reports) sends the token as a header.

// streamURL is what the renderer is told to play, from where in the item it starts (base), and its
// content type.
type streamURL struct {
	uri         string
	mime        string
	base        time.Duration
	transcoding bool
}

// withToken is u with the token added to its query.
func withToken(u, token string) string {
	if token == "" {
		return u
	}
	sep := "?"
	for i := 0; i < len(u); i++ {
		if u[i] == '?' {
			sep = "&"
			break
		}
	}
	return u + sep + "X-Plex-Token=" + url.QueryEscape(token)
}

// capProfile is what the device's decoder takes, as Plex's profile augmentation syntax: H.264 with
// AAC, MP3 or AC-3 sound in an MPEG-TS for HLS, no bigger than w by h, 8 bits.
func capProfile(w, h int) string {
	return "add-transcode-target(type=videoProfile&context=streaming&protocol=http" +
		"&container=mpegts&videoCodec=h264&audioCodec=aac,mp3,ac3&subtitleCodec=srt&replace=true)" +
		fmt.Sprintf("+add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.width&value=%d&replace=true)", w) +
		fmt.Sprintf("+add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.height&value=%d&replace=true)", h) +
		"+add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.bitDepth&value=8&replace=true)"
}

// audioStream is a song: the file as it is when the renderer decodes it and it starts at the
// beginning, else the server's MP3 transcode from offset (the renderer cannot seek, so a seek is a new
// transcode).
func (p *Player) audioStream(req mediaRequest, it *item, offset time.Duration, session string) streamURL {
	if mime := it.part.directMIME(); mime != "" && it.part.key != "" && offset == 0 {
		return streamURL{uri: withToken(req.rendererServer()+it.part.key, req.token), mime: mime}
	}
	q := url.Values{
		"path":                      {it.key},
		"protocol":                  {"http"},
		"directPlay":                {"0"},
		"directStream":              {"0"},
		"offset":                    {strconv.Itoa(int(offset / time.Second))},
		"session":                   {session},
		"musicBitrate":              {"320"},
		"X-Plex-Client-Identifier":  {p.ID.ClientID()},
		"X-Plex-Session-Identifier": {session},
		"X-Plex-Platform":           {plextv.Platform},
		"X-Plex-Product":            {plextv.Product},
	}
	u := req.rendererServer() + "/music/:/transcode/universal/start.mp3?" + q.Encode()
	return streamURL{uri: withToken(u, req.token), mime: "audio/mpeg", base: offset.Truncate(time.Second), transcoding: true}
}

// videoQuery is a film's transcode request: the server decides what to send (copying what it can),
// for the device's size, from offset, with subtitles burned in when on.
func (p *Player) videoQuery(it *item, offset time.Duration, subtitles bool, session string) url.Values {
	q := url.Values{
		"path":              {it.key},
		"mediaIndex":        {"0"},
		"partIndex":         {"0"},
		"protocol":          {"http"},
		"directPlay":        {"0"},
		"directStream":      {"1"},
		"fastSeek":          {"1"},
		"hasMDE":            {"1"},
		"location":          {"lan"},
		"subtitleSize":      {"100"},
		"audioBoost":        {"100"},
		"mediaBufferSize":   {"102400"},
		"autoAdjustQuality": {"0"},
		"offset":            {strconv.Itoa(int(offset / time.Second))},
		"session":           {session},

		"X-Plex-Client-Identifier":    {p.ID.ClientID()},
		"X-Plex-Session-Identifier":   {session},
		"X-Plex-Platform":             {plextv.Platform},
		"X-Plex-Product":              {plextv.Product},
		"X-Plex-Version":              {p.ID.Version()},
		"X-Plex-Device":               {p.ID.Device()},
		"X-Plex-Device-Name":          {p.ID.Name()},
		"X-Plex-Client-Profile-Name":  {"generic"},
		"X-Plex-Client-Profile-Extra": {capProfile(p.VideoW, p.VideoH)},
	}
	if a := it.part.selected(streamAudio); a != 0 {
		q.Set("audioStreamID", strconv.Itoa(a))
	}
	if s := it.part.selected(streamSubtitle); subtitles && s != 0 {
		q.Set("subtitles", "burn")
		q.Set("advancedSubtitles", "burn")
		q.Set("subtitleStreamID", strconv.Itoa(s))
	} else {
		q.Set("subtitles", "none")
	}
	return q
}

// videoStream asks the server for its decision (a start without one for the same session is refused
// with a 400) and answers its continuous MPEG-TS stream from offset. Not HLS: the server's playlist
// covers the whole item from 0:00 and marks the offset only with EXT-X-START, which the device's player
// does not follow, and its sending it there afterwards had the server convert from 0:00 and then
// again from the offset, with a long, stuttering start. A seek in this stream is a new one from the
// place (the renderer cannot move through it).
func (p *Player) videoStream(ctx context.Context, req mediaRequest, it *item, offset time.Duration, subtitles bool, session string) (streamURL, error) {
	q := p.videoQuery(it, offset, subtitles, session).Encode()
	if err := p.decide(ctx, req, "/video/:/transcode/universal/decision?"+q); err != nil {
		return streamURL{}, err
	}
	u := req.rendererServer() + "/video/:/transcode/universal/start?" + q
	return streamURL{uri: withToken(u, req.token), mime: "video/mp2t", base: offset.Truncate(time.Second), transcoding: true}, nil
}

func (p *Player) decide(ctx context.Context, req mediaRequest, path string) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, req.server()+path, nil)
	if err != nil {
		return err
	}
	p.serverHeaders(r, req.token)
	r.Header.Set("Accept", "application/xml")
	resp, err := p.Fetch.Do(r)
	if err != nil {
		return scrub(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("the server would not decide on a stream: %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, mostMetadata))
	return nil
}

// stopTranscode ends a stream the server is making for this player. Best effort.
func (p *Player) stopTranscode(req mediaRequest, video bool, session string) {
	if session == "" || req.address == "" {
		return
	}
	kind := "music"
	if video {
		kind = "video"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet,
		req.server()+"/"+kind+"/:/transcode/universal/stop?session="+url.QueryEscape(session), nil)
	if err != nil {
		return
	}
	p.serverHeaders(r, req.token)
	if resp, err := p.Fetch.Do(r); err == nil {
		resp.Body.Close()
	}
}

// artURL is the item's picture, scaled by the server (the renderer takes up to 4 MB), with the token,
// since the renderer fetches it too.
func artURL(req mediaRequest, thumb string) string {
	if thumb == "" {
		return ""
	}
	q := url.Values{"width": {"500"}, "height": {"500"}, "minSize": {"1"}, "upscale": {"1"}, "url": {thumb}}
	return withToken(req.rendererServer()+"/photo/:/transcode?"+q.Encode(), req.token)
}
