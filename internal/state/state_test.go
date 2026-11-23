package state

import (
	"os"
	"path/filepath"
	"testing"
)

// The state file is the owner's alone, keeps its client identifier, and comes back as it was saved.
func TestTheFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plex")
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := st.Get().ClientID
	if len(id) != 36 {
		t.Fatalf("client identifier %q", id)
	}
	if err := st.Update(func(s *State) { s.Token, s.User, s.ClientID = "tok", "kim", "" }); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := again.Get(); g.ClientID != id || g.Token != "tok" || g.User != "kim" || !g.Linked() {
		t.Errorf("read back %+v", g)
	}
}
