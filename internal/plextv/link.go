package plextv

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/redact"
	"github.com/HuskerMinion/techo5-plex/internal/state"
)

// The link: plex.tv hands the device four characters, somebody types them at plex.tv/link, and the
// account's token comes back on the poll.

// Status is what the link page shows.
type Status struct {
	Linked  bool   `json:"linked"`
	User    string `json:"user,omitempty"`
	Code    string `json:"code,omitempty"`
	Waiting bool   `json:"waiting,omitempty"`
	Err     string `json:"err,omitempty"`
}

// Linker runs the PIN flow and keeps the account in the state file.
type Linker struct {
	C     *Client
	Store *state.Store
	// URIs are the addresses a controller can reach the player at, for the account's device list.
	URIs func() []string
	// Poll and For are the pause between polls and how long a code is polled for.
	Poll, For time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	st     Status
}

// NewLinker is a linker with Plex's own timings: a poll every two seconds, for five minutes.
func NewLinker(c *Client, store *state.Store, uris func() []string) *Linker {
	if t := store.Get().Token; t != "" {
		redact.Secret(t)
	}
	return &Linker{C: c, Store: store, URIs: uris, Poll: 2 * time.Second, For: 5 * time.Minute}
}

// Status is a copy of the link's status.
func (l *Linker) Status() Status {
	l.mu.Lock()
	st := l.st
	l.mu.Unlock()
	s := l.Store.Get()
	st.Linked = s.Linked()
	if st.Linked {
		st.User = s.User
		st.Code, st.Waiting = "", false
	}
	return st
}

// errExpired is the code running out.
var errExpired = errors.New("the code expired before it was entered")

// Start asks plex.tv for a code and polls for the token. Asking again while a code is out answers the
// same code.
func (l *Linker) Start() (string, error) {
	l.mu.Lock()
	if l.st.Waiting && l.st.Code != "" {
		code := l.st.Code
		l.mu.Unlock()
		return code, nil
	}
	if l.cancel != nil {
		l.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.st = Status{Waiting: true}
	l.mu.Unlock()

	cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
	pin, err := l.C.CreatePin(cctx)
	ccancel()
	if err != nil {
		l.finish(err)
		return "", err
	}
	l.mu.Lock()
	l.st.Code = pin.Code
	l.mu.Unlock()
	// The code is for the owner to read: it is useless after the five minutes and to anyone without
	// the account's password, so it goes in the log for a device whose page is not at hand.
	slog.Info("plex: to link, enter this code at "+LinkURL, "code", pin.Code)
	go l.poll(ctx, pin.PinID())
	return pin.Code, nil
}

func (l *Linker) poll(ctx context.Context, id string) {
	deadline := time.Now().Add(l.For)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.Poll):
		}
		if time.Now().After(deadline) {
			l.finish(errExpired)
			return
		}
		pin, err := l.C.CheckPin(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var se StatusError
			if errors.As(err, &se) && se.Code == 404 {
				l.finish(errExpired)
				return
			}
			slog.Debug("plex: the PIN poll failed, trying again", "err", err)
			continue
		}
		if pin.AuthToken == "" {
			continue
		}
		l.linked(ctx, pin.AuthToken)
		return
	}
}

func (l *Linker) linked(ctx context.Context, token string) {
	redact.Secret(token)
	uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	user, uid, err := l.C.User(uctx, token)
	cancel()
	if err != nil {
		slog.Warn("plex: the account's name could not be read", "err", err)
	}
	if err := l.Store.Update(func(s *state.State) { s.Token, s.User, s.UserID = token, user, uid }); err != nil {
		slog.Error("plex: saving the account failed", "err", err)
		l.finish(err)
		return
	}
	l.mu.Lock()
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	l.st = Status{}
	l.mu.Unlock()
	slog.Info("plex: linked", "user", user)
	go l.Register(context.Background())
}

func (l *Linker) finish(err error) {
	msg := ""
	switch {
	case err == nil, errors.Is(err, context.Canceled):
	case errors.Is(err, errExpired):
		msg = "The code expired. Ask for a new one."
	default:
		msg = "plex.tv could not be reached."
		slog.Warn("plex: linking failed", "err", err)
	}
	l.mu.Lock()
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	l.st = Status{Err: msg}
	l.mu.Unlock()
}

// Cancel stops a link being made.
func (l *Linker) Cancel() {
	l.mu.Lock()
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	l.st = Status{}
	l.mu.Unlock()
}

// Unlink forgets the account and, as far as plex.tv lets go, the device's place in it. The client
// identifier stays: it is the device's name, not the account's.
func (l *Linker) Unlink() error {
	l.Cancel()
	token := l.Store.Get().Token
	err := l.Store.Update(func(s *state.State) { s.Token, s.User, s.UserID = "", "", "" })
	if token != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if uerr := l.C.Unregister(ctx, token); uerr != nil {
			slog.Debug("plex: unregistering the player failed", "err", uerr)
		}
		cancel()
	}
	slog.Info("plex: unlinked")
	return err
}

// Register tells the account where the player is: its device number is looked up, and the addresses
// published against it. Called after a link, at start, and when the address changes.
func (l *Linker) Register(ctx context.Context) {
	token := l.Store.Get().Token
	if token == "" || l.URIs == nil {
		return
	}
	uris := l.URIs()
	if len(uris) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := l.C.DeviceID(ctx, token)
	if err != nil {
		slog.Warn("plex: the account's device list could not be read", "err", err)
		return
	}
	if id == "" {
		slog.Info("plex: the account has no record of this player yet")
		return
	}
	if err := l.C.Register(ctx, token, id, uris); err != nil {
		slog.Warn("plex: the account was not told where the player is", "err", err)
		return
	}
	slog.Info("plex: the account knows where the player is", "addresses", len(uris))
}
