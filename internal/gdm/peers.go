package gdm

import (
	"bytes"
	"net"
	"slices"
	"sync"
	"time"
)

// The bounds on what Peers keeps: a home has a server or two.
const (
	mostPeers = 16
	seenFor   = 24 * time.Hour
)

// Peers are the Plex Media Servers announced to by unicast: those the linked account lists with a
// local address, and those a cast came from. Only private IPv4 addresses are kept: GDM is IPv4, and
// nothing a controller names makes the player send to the internet.
type Peers struct {
	mu      sync.Mutex
	account []net.IP
	seen    map[string]time.Time
	now     func() time.Time
}

// usable is whether ip may be announced to.
func usable(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4.IsPrivate()
}

// SetAccount replaces the servers the account lists, and says whether the list changed.
func (p *Peers) SetAccount(ips []net.IP) bool {
	var keep []net.IP
	for _, ip := range ips {
		if usable(ip) && !slices.ContainsFunc(keep, ip.To4().Equal) && len(keep) < mostPeers {
			keep = append(keep, ip.To4())
		}
	}
	slices.SortFunc(keep, func(a, b net.IP) int { return bytes.Compare(a, b) })
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := !slices.EqualFunc(keep, p.account, net.IP.Equal)
	p.account = keep
	return changed
}

// Saw notes a server a cast came from, and says whether it is new.
func (p *Peers) Saw(ip net.IP) bool {
	if !usable(ip) {
		return false
	}
	key := ip.To4().String()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[string]time.Time{}
	}
	now := p.clock()
	p.expireLocked(now)
	_, had := p.seen[key]
	if !had && len(p.seen) >= mostPeers {
		return false
	}
	p.seen[key] = now
	return !had && !slices.ContainsFunc(p.account, ip.To4().Equal)
}

// List is every server to announce to, each once, in address order.
func (p *Peers) List() []net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireLocked(p.clock())
	out := slices.Clone(p.account)
	for k := range p.seen {
		ip := net.ParseIP(k).To4()
		if !slices.ContainsFunc(out, ip.Equal) {
			out = append(out, ip)
		}
	}
	slices.SortFunc(out, func(a, b net.IP) int { return bytes.Compare(a, b) })
	return out
}

func (p *Peers) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Peers) expireLocked(now time.Time) {
	for k, at := range p.seen {
		if now.Sub(at) > seenFor {
			delete(p.seen, k)
		}
	}
}
