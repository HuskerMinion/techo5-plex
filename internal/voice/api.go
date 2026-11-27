package voice

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/pkg/addonapi"

	"github.com/HuskerMinion/techo5-plex/internal/companion"
)

// The connection to TECHO5's add-on API (docs/addon-api.md in TECHO5): the socket the daemon gives in
// TECHO5_ADDON_API, a hello, and then the command events, each answered within the 4 seconds Home
// Assistant waits.

// ReplyWithin is how long a command has before it is answered with Pending and carries on: Home
// Assistant waits 4 seconds for the answer.
var ReplyWithin = 3200 * time.Millisecond

// commandFor bounds a command that carries on after its answer.
const commandFor = 45 * time.Second

// Media is the device's player over the API, for companion.Local: the client of the connection up
// now, refused while there is none.
type Media struct {
	mu sync.Mutex
	c  *addonapi.Client
}

func (m *Media) set(c *addonapi.Client) {
	m.mu.Lock()
	m.c = c
	m.mu.Unlock()
}

func (m *Media) client() (*addonapi.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.c == nil {
		return nil, &MediaError{Say: "The Plex add-on isn't connected to the device yet.", Err: addonapi.ErrClosed}
	}
	return m.c, nil
}

func (m *Media) PlayVideo(ctx context.Context, url, title string) (string, error) {
	c, err := m.client()
	if err != nil {
		return "", err
	}
	id, err := c.StartMedia(ctx, addonapi.MediaVideo, url, title)
	return id, refusal(err)
}

// SetEntity sets one of the add-on's entities, for voice.Entities.
func (m *Media) SetEntity(ctx context.Context, id string, state any) error {
	c, err := m.client()
	if err != nil {
		return err
	}
	return c.SetEntity(ctx, id, state)
}

func (m *Media) Pause(ctx context.Context) error {
	c, err := m.client()
	if err != nil {
		return err
	}
	return refusal(c.PauseMedia(ctx))
}

func (m *Media) Resume(ctx context.Context) error {
	c, err := m.client()
	if err != nil {
		return err
	}
	return refusal(c.ResumeMedia(ctx))
}

func (m *Media) Stop(ctx context.Context) error {
	c, err := m.client()
	if err != nil {
		return err
	}
	return refusal(c.StopMedia(ctx))
}

// refusal is the daemon's refusal as companion.ErrGone (the add-on's item is not what plays) or as
// what to say.
func refusal(err error) error {
	if err == nil {
		return nil
	}
	var ae *addonapi.Error
	if !errors.As(err, &ae) {
		return err
	}
	say := ""
	switch ae.Code {
	case addonapi.CodeBadRequest:
		return companion.ErrGone
	case addonapi.CodeBusy:
		say = "It's quiet hours on this device."
	case addonapi.CodeRateLimited:
		say = "That's a lot of plays in a row. Try again in half a minute."
	case addonapi.CodeDenied:
		say = "The Plex add-on isn't allowed to play media on this device."
	case addonapi.CodeUnavailable:
		say = "This device can't play Plex video right now."
		if ae.Message != "" && len(ae.Message) <= 160 {
			say += " " + ae.Message
		}
	default:
		say = "The device couldn't play it."
	}
	return &MediaError{Say: say, Err: err}
}

// Serve connects to the API at socket ("" for TECHO5_ADDON_API) and answers its commands until ctx
// ends, connecting again after the daemon goes away.
func (v *Voice) Serve(ctx context.Context, socket string, media *Media) {
	wait, said := 2*time.Second, false
	for ctx.Err() == nil {
		c, err := addonapi.Dial(ctx, socket)
		if err != nil {
			// Said once: an add-on's log is a file on the flash.
			if !said {
				slog.Warn("plex: the add-on API is not answering; voice commands wait for it", "err", err)
				said = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, time.Minute)
			continue
		}
		w := c.Welcome()
		slog.Info("plex: voice commands on", "api", w.API,
			"media", slices.Contains(w.Capabilities, addonapi.CapMedia))
		wait, said = 2*time.Second, false
		states := c.WatchMedia()
		media.set(c)
		go v.follow(states)
		// What the device did with what voice played while the add-on was not connected.
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		mark := v.local.Mark()
		if st, err := c.MediaStatus(sctx); err == nil {
			v.local.Resync(mediaState(st), mark)
		} else {
			slog.Debug("plex: media.status was not answered", "err", err)
		}
		cancel()
		v.Publish() // the device keeps no entity's state across a connection
		v.serve(ctx, c)
		media.set(nil)
		c.Close()
		if ctx.Err() == nil {
			slog.Info("plex: the add-on API went away", "err", c.Err())
		}
	}
}

// follow hands the device's media.state events to the local player until the connection ends.
func (v *Voice) follow(states <-chan addonapi.MediaState) {
	for st := range states {
		v.local.State(mediaState(st))
	}
}

// mediaState is the API's media.state as the local player takes it.
func mediaState(st addonapi.MediaState) companion.MediaState {
	pos, ok := st.Position()
	return companion.MediaState{Media: st.Media, State: st.State, Reason: st.Reason, Position: pos, HasPosition: ok}
}

func (v *Voice) serve(ctx context.Context, c *addonapi.Client) {
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.Done():
		}
	}()
	for ev := range c.Events() {
		if ev.Event != addonapi.EventCommand {
			continue
		}
		var ce addonapi.CommandEvent
		if err := ev.Decode(&ce); err != nil {
			slog.Debug("plex: a command that does not read", "err", err)
			continue
		}
		go v.answer(c, ce)
	}
}

// answer carries out a command and answers it: with what it says, or, still at it when Home Assistant
// stops waiting, with Pending, while it carries on.
func (v *Voice) answer(c *addonapi.Client, ce addonapi.CommandEvent) {
	slog.Debug("plex: voice command", "command", ce.Command, "from", ce.From)
	// if_known: a "play ..." Home Assistant would otherwise send to music. Not a title in the library,
	// it is answered with nothing at once, and nothing plays, so music can have it.
	if ce.Command == CmdPlay && flag(ce.Args, ArgIfKnown) && !v.Known(str(ce.Args, "title")) {
		if ce.Call != 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := c.ReplyCommand(ctx, ce.Call, true, ""); err != nil {
				slog.Debug("plex: a command's answer was not taken", "err", err)
			}
		}
		return
	}
	done := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandFor)
		defer cancel()
		done <- v.Command(ctx, ce.Command, ce.Args)
	}()
	if ce.Call == 0 {
		return // a button: nothing waits for the answer
	}
	var text string
	select {
	case text = <-done:
	case <-time.After(ReplyWithin):
		text = Pending(ce.Command, ce.Args)
		// The answer comes too late to be heard. A question in it was never asked, so it may not wait
		// for an answer: "resume" said for something else would start it.
		go func() {
			if late := <-done; strings.HasSuffix(late, "?") {
				v.answered()
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.ReplyCommand(ctx, ce.Call, true, text); err != nil {
		slog.Debug("plex: a command's answer was not taken", "err", err)
	}
}
