// Package pubsub holds the account's socket to plex.tv open. Plex's phone and TV apps list a player
// only while plex.tv says it is present, and present is having this socket open: a player found on
// the network alone is no longer enough for them. What comes down it are Plex's own messages; the
// format is not documented, so every frame is logged (token-free, cut short) until it is known, and a
// frame that names a player command is run against the Companion server's own handlers.
package pubsub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Base is plex.tv's socket address, before the account's number and the player's identifier.
const Base = "wss://pubsub.plex.tv/sub/websockets"

// Account is what a dial needs, read at each dial so an unlink is not reconnected with what it had.
type Account func() (userID, clientID, token string)

// Run holds the socket open until ctx ends, dialing again with a growing pause after it closes.
// handler takes the commands that come down it.
func Run(ctx context.Context, acct Account, handler http.Handler) {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		userID, clientID, token := acct()
		if token == "" || userID == "" {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}
		opened := time.Now()
		err := socket(ctx, userID, clientID, token, handler)
		if ctx.Err() != nil {
			return
		}
		if time.Since(opened) > time.Minute {
			backoff = 2 * time.Second
		}
		slog.Info("plex: the account's socket closed", "err", scrub(err), "again_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func socket(ctx context.Context, userID, clientID, token string, handler http.Handler) error {
	u := Base + "/" + url.PathEscape(userID) + "/" + url.PathEscape(clientID) + "?X-Plex-Token=" + url.QueryEscape(token)
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := d.DialContext(ctx, u, http.Header{"X-Plex-Client-Identifier": {clientID}, "X-Plex-Token": {token}})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	slog.Info("plex: the account's socket is open")
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetReadLimit(1 << 20)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		frame(msg, token, handler)
	}
}

// frame logs one message and runs a player command in it, if it is one. Two shapes are tried:
// {"path": "player/...", "params": {...}} and {"player/...": {...}}; and a Companion URL anywhere in a
// string ("/player/playback/pause?...").
func frame(msg []byte, token string, handler http.Handler) {
	text := strings.ReplaceAll(string(msg), token, "<token>")
	if len(text) > 600 {
		text = text[:600] + "…"
	}
	slog.Info("plex: a message on the account's socket", "msg", text)

	var m map[string]json.RawMessage
	if json.Unmarshal(msg, &m) != nil {
		return
	}
	path := ""
	var params json.RawMessage
	if raw, ok := m["path"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			path, params = s, m["params"]
		}
	}
	if path == "" {
		for k, raw := range m {
			if strings.HasPrefix(strings.TrimPrefix(k, "/"), "player/") {
				path, params = k, raw
				break
			}
		}
	}
	path = strings.TrimPrefix(path, "/")
	if !strings.HasPrefix(path, "player/") {
		return
	}
	q := url.Values{}
	var flat map[string]any
	if len(params) > 0 && json.Unmarshal(params, &flat) == nil {
		for k, v := range flat {
			if s, ok := v.(string); ok {
				q.Set(k, s)
				continue
			}
			if b, err := json.Marshal(v); err == nil {
				q.Set(k, strings.Trim(string(b), `"`))
			}
		}
	}
	r, err := http.NewRequest(http.MethodGet, "/"+path+"?"+q.Encode(), nil)
	if err != nil {
		return
	}
	// From plex.tv, not a browser: the Companion server's home-network check is for the network side.
	r.RemoteAddr, r.Host = "127.0.0.1:0", "127.0.0.1"
	slog.Info("plex: a command from the account's socket", "path", path)
	handler.ServeHTTP(discard{}, r)
}

func scrub(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "X-Plex-Token="); i >= 0 {
		s = s[:i] + "X-Plex-Token=<token>"
	}
	return s
}

type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}
