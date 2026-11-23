// Package plextv is the account side: what the device says it is on every call to Plex, the PIN link
// at plex.tv/link, and telling the account where the player is (so the phone apps that list only the
// account's players can find it).
//
// Adapted from kevin909867's plexcast (TECHO5 PR #691): pin.go, identity.go and cloud.go.
package plextv

import (
	"net/http"
	"sync"
)

const (
	// Product is the name Plex shows in an account's device list.
	Product = "TECHO5"

	// Platform is not a description but a key: the server picks the transcode profile from it, and a
	// name it does not know is answered with a bare 400 on every /transcode/universal route ("Linux"
	// among them). Generic is the stand-in profile; the capability profile adds what this hardware
	// takes to it.
	Platform = "Generic"

	// DeviceClass is what controllers draw the player as.
	DeviceClass = "tv"

	// Provides is what the device can be asked to do: what puts a "player" in the account's list.
	// pubsub-player: the account socket (internal/pubsub) is what makes plex.tv list the player as present.
	Provides = "client,player,pubsub-player"

	// Model is the hardware family, as the account records it.
	Model = "TECHO5"
)

// Identity is what the device says it is.
type Identity struct {
	mu       sync.RWMutex
	clientID string
	name     string
	version  string
	device   string
}

// NewIdentity is the device's identity. name may change later (SetName), when the renderer's own name
// is learned.
func NewIdentity(clientID, name, version, device string) *Identity {
	if version == "" {
		version = "1.0.0"
	}
	if device == "" {
		device = "Echo Show"
	}
	return &Identity{clientID: clientID, name: name, version: version, device: device}
}

func (id *Identity) ClientID() string { return id.clientID }
func (id *Identity) Version() string  { return id.version }
func (id *Identity) Device() string   { return id.device }

// Name is what Plex lists the device as.
func (id *Identity) Name() string {
	id.mu.RLock()
	defer id.mu.RUnlock()
	return id.name
}

// SetName changes it.
func (id *Identity) SetName(n string) {
	if n == "" {
		return
	}
	id.mu.Lock()
	id.name = n
	id.mu.Unlock()
}

// Headers are on every call to Plex. The token is never among them: callers add it themselves, as a
// header, where a call needs it.
func (id *Identity) Headers() map[string]string {
	return map[string]string{
		"Accept":                   "application/json",
		"X-Plex-Product":           Product,
		"X-Plex-Version":           id.Version(),
		"X-Plex-Platform":          Platform,
		"X-Plex-Device":            id.Device(),
		"X-Plex-Device-Name":       id.Name(),
		"X-Plex-Client-Identifier": id.ClientID(),
		"X-Plex-Provides":          Provides,
		"X-Plex-Model":             Model,
	}
}

// Apply sets the identity headers on req.
func (id *Identity) Apply(req *http.Request) {
	for k, v := range id.Headers() {
		req.Header.Set(k, v)
	}
}
