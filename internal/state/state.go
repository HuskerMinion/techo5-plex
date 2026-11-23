// Package state keeps what the add-on must remember across restarts: the client identifier Plex knows
// the device by, and the linked account (its token and name). One JSON file, mode 0600, written whole
// to a temporary file and renamed over the old one, so a power cut leaves the old file or the new one.
package state

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// State is the file's content.
type State struct {
	// ClientID is the X-Plex-Client-Identifier: made once and kept, because plex.tv binds the
	// account's token to it and every controller dedupes players on it.
	ClientID string `json:"clientIdentifier"`
	Token    string `json:"token,omitempty"`
	User     string `json:"user,omitempty"`
	UserID   string `json:"userID,omitempty"`
}

// Linked is whether an account is linked.
func (s State) Linked() bool { return s.Token != "" }

// Store is the file and a copy of it in memory.
type Store struct {
	path string
	mu   sync.Mutex
	s    State
}

// FileName is the file in the data directory.
const FileName = "state.json"

// Open reads the state in dir, making dir (0700) and a client identifier if there are none yet.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st := &Store{path: filepath.Join(dir, FileName)}
	b, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &st.s); err != nil {
			return nil, fmt.Errorf("%s is not readable: %w", st.path, err)
		}
	}
	if st.s.ClientID == "" {
		st.s.ClientID = NewUUID()
		if err := st.writeLocked(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// Get is a copy of the state.
func (st *Store) Get() State {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

// Update changes the state and writes it. The client identifier cannot be cleared.
func (st *Store) Update(fn func(*State)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.s
	fn(&next)
	if next.ClientID == "" {
		next.ClientID = st.s.ClientID
	}
	prev := st.s
	st.s = next
	if err := st.writeLocked(); err != nil {
		st.s = prev
		return err
	}
	return nil
}

func (st *Store) writeLocked() error {
	b, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), st.path)
}

// NewUUID is a random (version 4) UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
