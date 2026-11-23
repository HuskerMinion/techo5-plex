package companion

import (
	"context"
	"errors"
	"net/url"
	"time"
)

// errNotAccountServer refuses a film from a server the linked account does not list.
var errNotAccountServer = errors.New("films play only from a Plex server the linked account lists: link the account on the add-on's page")

// trustFor is how long an answer about a server is kept: a seek or a track change restarts the film,
// and need not ask plex.tv again each time.
const trustFor = 10 * time.Minute

type trusted struct {
	ok bool
	at time.Time
}

// accountServer is whether req's server is one the linked account lists, at an address the account
// lists for it (ServerToken). The device's renderer plays a film from a program on the device without
// asking on the screen, as one it trusts, and this program relays what any device on the network
// sends: so a film is played only from the account's own servers, never from a server a request
// names, which could be anyone's. Music is not held to this: the renderer plays music from any device
// on the network without asking anyway.
func (p *Player) accountServer(ctx context.Context, req mediaRequest) bool {
	if p.ServerToken == nil || req.machine == "" {
		return false
	}
	u, err := url.Parse(req.server())
	if err != nil || u.Hostname() == "" {
		return false
	}
	key := req.machine + "|" + u.Hostname()
	p.trustMu.Lock()
	t, ok := p.trust[key]
	p.trustMu.Unlock()
	if ok && time.Since(t.at) < trustFor {
		return t.ok
	}
	yes := p.ServerToken(ctx, req.machine, u.Hostname()) != ""
	p.trustMu.Lock()
	if p.trust == nil {
		p.trust = map[string]trusted{}
	}
	p.trust[key] = trusted{yes, time.Now()}
	p.trustMu.Unlock()
	return yes
}
