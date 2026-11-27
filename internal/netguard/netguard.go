// Package netguard holds the network rules every outgoing request from the add-on follows.
//
// Companion control is unauthenticated on the home network by Plex's design: anybody on the LAN can
// tell a player to fetch from an address of their choosing. So nothing a controller names is ever
// fetched from the device itself (loopback or one of its own addresses), from link-local or multicast
// addresses, or from nowhere, whatever name, redirect or DNS answer leads there. The check is made when
// the connection is dialed, so redirects and DNS rebinding are covered as well.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// OwnAddrs lists the device's own addresses. A variable so tests can stand in for the host's.
var OwnAddrs = func() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out
}

// OwnIP is whether ip is one of the device's own addresses, read at each call since the address can
// change with the network.
func OwnIP(ip net.IP) bool {
	for _, own := range OwnAddrs() {
		if own.Equal(ip) {
			return true
		}
	}
	return false
}

// Refused is whether a controller-named address must not be connected to.
func Refused(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || OwnIP(ip)
}

// Control is a net.Dialer Control that refuses the addresses Refused names.
func Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if Refused(net.ParseIP(host)) {
		return fmt.Errorf("not connecting to %s", host)
	}
	return nil
}

// Client is an HTTP client for addresses a controller named: quick to connect and to answer, with
// Control on every dial, and no more than three redirects. timeout bounds the whole request, 0 for
// none (a stream can run for minutes).
func Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           plexDirectDial((&net.Dialer{Timeout: 5 * time.Second, Control: Control}).DialContext),
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			// A token goes in a header, and Go carries custom headers on to wherever a redirect
			// leads: a redirect may not leave the host it started on.
			if req.URL.Host != via[0].URL.Host {
				return errors.New("a redirect to another host")
			}
			return nil
		},
	}
}

// Home is whether a request may come from ip: the home network's own ranges and the device itself,
// never the internet.
func Home(ip net.IP) bool {
	return ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback())
}

// RemoteIP is the address a request came from.
func RemoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// HostIsAddress is whether a request's Host is an IP address, with or without a port. A name there is
// a web page that pointed a name of its own at the device (DNS rebinding) to reach it from a browser.
func HostIsAddress(host string) bool {
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = trimZone(trimBrackets(host))
	// A plex.direct name stands for the address written in it, under Plex's own domain: no page can
	// point one somewhere else, so it is as good as the address. A Plex Media Server passing on a
	// controller's command (Plex Web) names itself that way.
	return net.ParseIP(host) != nil || PlexDirectIP(host) != nil
}

func trimBrackets(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}

func trimZone(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			return s[:i]
		}
	}
	return s
}

// InterfaceIPv4 is the first IPv4 address on the named interface, or nil.
func InterfaceIPv4(name string) net.IP {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			return n.IP.To4()
		}
	}
	return nil
}

// PlexDirectIP is the address a plex.direct name stands for, or nil for any other name. Plex hands out
// its servers' secure addresses as names like 192-168-1-20.<hash>.plex.direct (IPv6 with dashes for
// colons), whose certificate is the server's own. Many home DNS servers refuse to answer them, as
// protection against DNS rebinding (a public name for a private address), so the name is not looked
// up: the address is read from it, and TLS still checks the certificate against the name.
func PlexDirectIP(host string) net.IP {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !strings.HasSuffix(host, ".plex.direct") {
		return nil
	}
	label, _, _ := strings.Cut(host, ".")
	if ip := net.ParseIP(strings.ReplaceAll(label, "-", ".")); ip != nil && ip.To4() != nil {
		return ip
	}
	if ip := net.ParseIP(strings.ReplaceAll(label, "-", ":")); ip != nil {
		return ip
	}
	return nil
}

// plexDirectDial dials a plex.direct name at the address in it (PlexDirectIP), and anything else as
// dial would.
func plexDirectDial(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			if ip := PlexDirectIP(host); ip != nil {
				addr = net.JoinHostPort(ip.String(), port)
			}
		}
		return dial(ctx, network, addr)
	}
}
