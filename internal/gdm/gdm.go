// Package gdm is Plex's discovery ("Good Day Mate"), which is not SSDP: a controller sends a bare
// "M-SEARCH * HTTP/1.0" to 239.0.0.250:32412, and every player answers it directly, from the port the
// search came to, with a plain header block naming itself and its control port. A player also says it
// is here on its own (HELLO, to port 32413) when it starts and every so often after, and withdraws
// (BYE) when it stops.
//
// Multicast stays on the subnet it was sent on, so a Plex Media Server on another subnet never hears a
// HELLO, and Plex Web, which reaches players through the server, does not list the player. The same
// HELLO and BYE are sent by unicast to each server the player knows of as well (Peers).
//
// Adapted from kevin909867's plexcast (TECHO5 PR #691), gdm.go. The add-on joins the group on one
// interface, named explicitly (wlan0 on the device), rather than the kernel's default.
package gdm

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/HuskerMinion/techo5-plex/internal/netguard"
)

const (
	Group      = "239.0.0.250"
	SearchPort = 32412
	HelloPort  = 32413
	helloEvery = 30 * time.Second
)

// Info is what the player says about itself.
type Info struct {
	Name, ClientID, Product, Version, DeviceClass string
	Port                                          int
	ProtocolVersion                               int
	Capabilities                                  string
}

// Server answers searches and announces the player.
type Server struct {
	// Iface is the interface to join the group on and announce from ("" lets the kernel choose).
	Iface string
	// Info is read at every answer, so a name learned later is used.
	Info func() Info

	// For tests: the address to listen on, the group to join ("" for none) and where HELLO goes.
	Listen    string
	JoinGroup string
	HelloTo   *net.UDPAddr
	// OnListen, when set, is told the address listened on.
	OnListen func(net.Addr)

	// Peers, when set, are the Plex Media Servers HELLO and BYE also go to by unicast, at PeerPort.
	Peers func() []net.IP
	// PeerPort is HelloPort; tests change it.
	PeerPort int

	answering chan struct{}

	once      sync.Once
	kick      chan struct{}
	mu        sync.Mutex
	announced string // the peers last announced to, for the log
}

// New is a server for the real GDM ports.
func New(iface string, info func() Info) *Server {
	return &Server{
		Iface: iface, Info: info,
		Listen:    ":" + strconv.Itoa(SearchPort),
		JoinGroup: Group,
		HelloTo:   &net.UDPAddr{IP: net.ParseIP(Group), Port: HelloPort},
		PeerPort:  HelloPort,
	}
}

func (s *Server) kicks() chan struct{} {
	s.once.Do(func() { s.kick = make(chan struct{}, 1) })
	return s.kick
}

// Announce sends a HELLO now rather than at the next turn: a server has just been learned.
func (s *Server) Announce() {
	select {
	case s.kicks() <- struct{}{}:
	default:
	}
}

// Run answers searches until ctx ends, then says goodbye. Listening is tried until it works: at boot
// the network may not be up yet.
func (s *Server) Run(ctx context.Context) {
	s.answering = make(chan struct{}, 8)
	var conn *net.UDPConn
	for wait := 2 * time.Second; ; wait = min(wait*2, time.Minute) {
		c, err := s.listen()
		if err == nil {
			conn = c
			break
		}
		slog.Warn("plex: listening for GDM searches failed", "iface", s.Iface, "err", err, "again in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	if s.OnListen != nil {
		s.OnListen(conn.LocalAddr())
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	slog.Info("plex: answering GDM searches", "iface", s.Iface, "listen", s.Listen)

	go func() {
		kick := s.kicks()
		for {
			if in := s.Info(); in.Name != "" {
				s.tell(HelloPacket(in))
			}
			select {
			case <-ctx.Done():
				if in := s.Info(); in.Name != "" {
					s.tell(ByePacket(in))
				}
				return
			case <-time.After(helloEvery):
			case <-kick:
			}
		}
	}()

	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("plex: reading GDM searches failed", "err", err)
			}
			return
		}
		// A player with no name yet (Info.Name "") is not ready to play and is not offered.
		if !IsSearch(buf[:n]) || !netguard.Home(from.IP) || from.Port == 0 || s.Info().Name == "" {
			slog.Debug("plex: a datagram on the search port was not answered", "from", from.IP.String())
			continue
		}
		select {
		case s.answering <- struct{}{}:
			go func() {
				defer func() { <-s.answering }()
				if _, err := conn.WriteToUDP([]byte(Answer(s.Info())), from); err != nil && ctx.Err() == nil {
					slog.Debug("plex: answering a search failed", "err", err)
					return
				}
				slog.Debug("plex: answered a GDM search", "from", from.IP.String())
			}()
		default: // more searches than any home sends at once: the rest go unanswered
		}
	}
}

func (s *Server) listen() (*net.UDPConn, error) {
	pc, err := net.ListenPacket("udp4", s.Listen)
	if err != nil {
		return nil, err
	}
	conn := pc.(*net.UDPConn)
	if s.JoinGroup == "" {
		return conn, nil
	}
	var ifi *net.Interface
	if s.Iface != "" {
		if ifi, err = net.InterfaceByName(s.Iface); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if err := ipv4.NewPacketConn(conn).JoinGroup(ifi, &net.UDPAddr{IP: net.ParseIP(s.JoinGroup)}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// tell sends a packet to the HELLO port, multicast and to each peer, from the interface's address so it
// leaves on that interface.
func (s *Server) tell(packet string) {
	var laddr *net.UDPAddr
	if s.Iface != "" {
		ip := netguard.InterfaceIPv4(s.Iface)
		if ip == nil {
			return
		}
		laddr = &net.UDPAddr{IP: ip}
	}
	if s.HelloTo != nil {
		send(laddr, s.HelloTo, packet)
	}
	if s.Peers == nil {
		return
	}
	port := s.PeerPort
	if port == 0 {
		port = HelloPort
	}
	peers := s.Peers()
	names := make([]string, 0, len(peers))
	for _, ip := range peers {
		names = append(names, ip.String())
		send(laddr, &net.UDPAddr{IP: ip, Port: port}, packet)
	}
	list := strings.Join(names, ",")
	s.mu.Lock()
	changed := list != s.announced
	s.announced = list
	s.mu.Unlock()
	if changed {
		if list == "" {
			slog.Info("plex: no Plex Media Servers to announce to by unicast")
		} else {
			slog.Info("plex: announcing to Plex Media Servers by unicast", "servers", list)
		}
	}
}

func send(laddr, to *net.UDPAddr, packet string) {
	conn, err := net.DialUDP("udp4", laddr, to)
	if err != nil {
		slog.Debug("plex: announcing failed", "to", to.IP.String(), "err", err)
		return
	}
	_, _ = conn.Write([]byte(packet))
	conn.Close()
}

// IsSearch is whether a datagram is Plex's search. Senders differ on the version, on CRLF against LF,
// and on how many blank lines they send, so only the first line is read.
func IsSearch(b []byte) bool {
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimRight(line, "\r")
	return strings.HasPrefix(line, "M-SEARCH * HTTP/")
}

// Answer is the reply to a search.
func Answer(i Info) string { return "HTTP/1.0 200 OK\r\n" + headers(i) + "\r\n" }

// HelloPacket is the same block announced.
func HelloPacket(i Info) string { return "HELLO * HTTP/1.0\r\n" + headers(i) + "\r\n" }

// ByePacket is the announcement withdrawn.
func ByePacket(i Info) string { return "BYE * HTTP/1.0\r\n" + headers(i) + "\r\n" }

// headers is the block every packet carries, in the order Plex's own players write it.
func headers(i Info) string {
	return "Content-Type: plex/media-player\r\n" +
		"Name: " + Header(i.Name) + "\r\n" +
		"Port: " + strconv.Itoa(i.Port) + "\r\n" +
		"Product: " + Header(i.Product) + "\r\n" +
		"Protocol: plex\r\n" +
		"Protocol-Capabilities: " + Header(i.Capabilities) + "\r\n" +
		"Protocol-Version: " + strconv.Itoa(i.ProtocolVersion) + "\r\n" +
		"Resource-Identifier: " + Header(i.ClientID) + "\r\n" +
		"Version: " + Header(i.Version) + "\r\n" +
		"Device-Class: " + Header(i.DeviceClass) + "\r\n"
}

// Header is a value with what a header cannot carry taken out: a newline in the device's name would
// otherwise be a second header.
func Header(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, s)
}
