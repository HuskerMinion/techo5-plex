// techo5-plex makes a TECHO5 device a Plex player: casting apps find it on the home network (GDM) or
// through the linked Plex account, and what they cast is played by the device's own DLNA renderer.
//
// It is an add-on, run from userdata at boot (see README.md). It is based on kevin909867's plexcast,
// TECHO5 PR #691.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/companion"
	"github.com/HuskerMinion/techo5-plex/internal/dlna"
	"github.com/HuskerMinion/techo5-plex/internal/gdm"
	"github.com/HuskerMinion/techo5-plex/internal/linkpage"
	"github.com/HuskerMinion/techo5-plex/internal/netguard"
	"github.com/HuskerMinion/techo5-plex/internal/plextv"
	"github.com/HuskerMinion/techo5-plex/internal/pubsub"
	"github.com/HuskerMinion/techo5-plex/internal/redact"
	"github.com/HuskerMinion/techo5-plex/internal/state"
)

// version is stamped by build.sh.
var version = "0.1.0-dev"

func main() {
	var (
		dataDir   = flag.String("data", "/data/techo5-linux/plex", "where the state file (client identifier, account) is kept")
		iface     = flag.String("iface", "wlan0", "the network interface to answer GDM searches and announce on")
		renderer  = flag.String("renderer", dlna.DefaultDescription, "the DLNA renderer's description URL (TECHO5's own, on 127.0.0.1)")
		name      = flag.String("name", "", "the name Plex lists the player as (default: the renderer's name)")
		port      = flag.Int("port", companion.Port, "the Companion control port")
		pagePort  = flag.Int("page-port", linkpage.Port, "the link page's port (0 for none)")
		videoSize = flag.String("video-size", "1280x720", "the largest picture asked of the server (1920x1080 suits the Skylight)")
		device    = flag.String("device", "Echo Show", "the device model the account records")
		debug     = flag.Bool("debug", false, "log at debug level")
		showVer   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(redact.Handler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))))

	store, err := state.Open(*dataDir)
	if err != nil {
		slog.Error("plex: the state file could not be opened", "dir", *dataDir, "err", err)
		os.Exit(1)
	}
	var vw, vh int
	if _, err := fmt.Sscanf(*videoSize, "%dx%d", &vw, &vh); err != nil || vw < 160 || vh < 120 {
		slog.Error("plex: -video-size is WIDTHxHEIGHT", "got", *videoSize)
		os.Exit(2)
	}

	id := plextv.NewIdentity(store.Get().ClientID, or(*name, "TECHO5"), version, *device)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// named is whether the player has its name: the one given with -name, or the DLNA renderer's once it
	// is found. Until then it is not offered to Plex apps: with DLNA off it could not play anything, and
	// it would be listed only as "TECHO5", which tells two devices apart no better than not at all.
	var named atomic.Bool
	named.Store(*name != "")
	rf := &rendererFinder{url: *renderer}
	go rf.run(ctx, func(n string) {
		if *name == "" {
			id.SetName(n)
		}
		named.Store(true)
	})

	player := companion.New(id, rf.get)
	player.VideoW, player.VideoH = vw, vh

	uris := func() []string {
		ip := netguard.InterfaceIPv4(*iface)
		if ip == nil {
			return nil
		}
		return []string{"http://" + net.JoinHostPort(ip.String(), strconv.Itoa(*port))}
	}
	account := plextv.NewClient(id)
	linker := plextv.NewLinker(account, store, uris)

	// Plex Media Servers on another subnet than the device's never hear multicast: they are
	// announced to by unicast too, those the account lists and those casts come from.
	peers := &gdm.Peers{}
	g := gdm.New(*iface, func() gdm.Info {
		if !named.Load() {
			return gdm.Info{}
		}
		return gdm.Info{Name: id.Name(), ClientID: id.ClientID(), Product: plextv.Product, Version: id.Version(),
			DeviceClass: plextv.DeviceClass, Port: *port, ProtocolVersion: companion.ProtocolVersion,
			Capabilities: companion.Capabilities}
	})
	g.Peers = peers.List
	player.ServerToken = func(ctx context.Context, machine, host string) string {
		token := store.Get().Token
		if token == "" || machine == "" {
			return ""
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		access, hosts, err := account.ServerAccess(cctx, token, machine)
		if err != nil {
			slog.Debug("plex: no account token for the server", "err", err)
			return ""
		}
		if !slices.Contains(hosts, strings.ToLower(host)) {
			slog.Debug("plex: the account does not list that address for the server")
			return ""
		}
		return access
	}
	player.OnServer = func(ip net.IP) {
		if peers.Saw(ip) {
			g.Announce()
		}
	}

	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}
	run(func() {
		if err := player.Serve(ctx, ":"+strconv.Itoa(*port)); err != nil {
			slog.Error("plex: the Companion server stopped", "err", err)
			stop()
		}
	})
	run(func() { player.Run(ctx) })
	run(func() { g.Run(ctx) })
	run(func() { serversLoop(ctx, account, store, peers, g.Announce) })
	// The renderer's events: Next and Previous on the device's own screen, and state changes sooner.
	events := &dlna.Events{URL: rf.eventURL, On: player.Event}
	run(func() {
		if err := events.Run(ctx); err != nil {
			slog.Warn("plex: the renderer's events cannot be listened for", "err", err)
		}
	})
	if *pagePort > 0 {
		page := linkpage.New(linker, id.Name, rf.problem, player.Busy)
		run(func() {
			slog.Info("plex: link page on", "port", *pagePort)
			if err := page.Serve(ctx, ":"+strconv.Itoa(*pagePort)); err != nil {
				slog.Error("plex: the link page stopped", "err", err)
			}
		})
	}
	run(func() { registerLoop(ctx, linker, uris) })
	run(func() {
		pubsub.Run(ctx, func() (string, string, string) {
			st := store.Get()
			return st.UserID, id.ClientID(), st.Token
		}, player.Handler())
	})

	st := store.Get()
	if st.Linked() {
		slog.Info("plex: started", "version", version, "linked", true, "user", st.User)
	} else {
		slog.Info("plex: started, not linked: open the link page to link an account", "version", version,
			"page", pageAddr(*iface, *pagePort))
	}
	<-ctx.Done()
	wg.Wait()
}

// registerLoop tells the account where the player is at start, when the address changes, and every
// hour (the account forgets connections it has not heard of for a while).
func registerLoop(ctx context.Context, l *plextv.Linker, uris func() []string) {
	last, linked, at := "", false, time.Time{}
	for {
		cur := fmt.Sprint(uris())
		now := l.Store.Get().Linked()
		if now && (cur != last || !linked || time.Since(at) > time.Hour) {
			l.Register(ctx)
			at = time.Now()
		}
		last, linked = cur, now
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// serversLoop learns the Plex Media Servers the linked account lists (their local addresses), on link
// and every ten minutes, and forgets them on unlink.
func serversLoop(ctx context.Context, c *plextv.Client, store *state.Store, peers *gdm.Peers, announce func()) {
	last, at := "", time.Time{}
	for {
		token := store.Get().Token
		switch {
		case token == "":
			last = ""
			if peers.SetAccount(nil) {
				announce()
			}
		case token != last || time.Since(at) >= 10*time.Minute:
			at, last = time.Now(), token
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			ips, err := c.LocalServers(cctx, token)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("plex: the account's servers could not be listed", "err", err)
				at = time.Now().Add(-9 * time.Minute) // again in a minute
				break
			}
			slog.Debug("plex: the account's servers", "local", len(ips))
			if peers.SetAccount(ips) {
				announce()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func pageAddr(iface string, port int) string {
	ip := netguard.InterfaceIPv4(iface)
	if ip == nil || port == 0 {
		return ""
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(port)) + "/"
}

// rendererFinder looks for the device's DLNA renderer until it answers, and again whenever a call to
// it fails to connect (TECHO5 restarting, DLNA turned off and on).
type rendererFinder struct {
	url string

	mu  sync.Mutex
	r   *dlna.Renderer
	why string
}

func (f *rendererFinder) get() companion.Renderer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.r == nil {
		return nil
	}
	return f.r
}

// eventURL is the renderer's AVTransport event URL, "" while it is not found.
func (f *rendererFinder) eventURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.r == nil {
		return ""
	}
	return f.r.AVTEvents
}

// problem is "" while the renderer is found, else what the owner should do.
func (f *rendererFinder) problem() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.r != nil {
		return ""
	}
	if f.why == "" {
		return "Looking for the device's DLNA renderer…"
	}
	return "The device's DLNA renderer is not answering: turn on DLNA in TECHO5 (and Video and DLNA video for films). " + f.why
}

func (f *rendererFinder) run(ctx context.Context, named func(string)) {
	for {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		r, err := dlna.Discover(dctx, nil, f.url)
		cancel()
		f.mu.Lock()
		had := f.r != nil
		if err != nil {
			f.r, f.why = nil, err.Error()
		} else {
			f.r, f.why = r, ""
		}
		f.mu.Unlock()
		switch {
		case err != nil && had:
			slog.Warn("plex: the DLNA renderer went away", "err", err)
		case err != nil:
			slog.Debug("plex: the DLNA renderer was not found", "err", err)
		case !had:
			slog.Info("plex: DLNA renderer found", "name", r.Name)
			named(r.Name)
		}
		wait := time.Minute
		if err != nil {
			wait = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
