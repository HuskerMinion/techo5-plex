package dlna

import (
	"strings"
	"time"
)

// Meta is what the renderer is told about an item: what Now Playing shows, and whether it is music or
// video (TECHO5's renderer reads upnp:class first).
type Meta struct {
	Title, Artist, Album string
	Art                  string // album art URL; may carry a token
	Dur                  time.Duration
	Video                bool
	MIME                 string // the res element's content type
	// Start is where in the whole item the stream begins, for a film sent again from a place: Dur
	// is then the rest of it. Told to TECHO5 as its own start attribute on res, so its screen can
	// show the film's own time; other renderers ignore it.
	Start time.Duration
	URI   string // the res element's URL; may carry a token
}

// DIDL is the item as DIDL-Lite, the description SetAVTransportURI carries.
func DIDL(m Meta) string {
	class := "object.item.audioItem.musicTrack"
	if m.Video {
		class = "object.item.videoItem"
	}
	mime := m.MIME
	if mime == "" {
		mime = "*"
	}
	var b strings.Builder
	b.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"` +
		` xmlns:dc="http://purl.org/dc/elements/1.1/"` +
		` xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"` +
		` xmlns:techo5="urn:techo5:dlna">`)
	b.WriteString(`<item id="plex" parentID="0" restricted="1">`)
	b.WriteString(`<dc:title>` + Escape(orDefault(m.Title, "Plex")) + `</dc:title>`)
	if m.Artist != "" {
		b.WriteString(`<upnp:artist>` + Escape(m.Artist) + `</upnp:artist>`)
		b.WriteString(`<dc:creator>` + Escape(m.Artist) + `</dc:creator>`)
	}
	if m.Album != "" {
		b.WriteString(`<upnp:album>` + Escape(m.Album) + `</upnp:album>`)
	}
	if m.Art != "" {
		b.WriteString(`<upnp:albumArtURI>` + Escape(m.Art) + `</upnp:albumArtURI>`)
	}
	b.WriteString(`<upnp:class>` + class + `</upnp:class>`)
	b.WriteString(`<res protocolInfo="http-get:*:` + Escape(mime) + `:*"`)
	if m.Dur > 0 {
		b.WriteString(` duration="` + HMS(m.Dur) + `.000"`)
	}
	if m.Start > 0 {
		b.WriteString(` techo5:start="` + HMS(m.Start) + `"`)
	}
	b.WriteString(`>` + Escape(m.URI) + `</res>`)
	b.WriteString(`</item></DIDL-Lite>`)
	return b.String()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
