package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// A token never reaches the log: not as a registered value, not in a query string, not in an error.
func TestTokensAreRedacted(t *testing.T) {
	var b bytes.Buffer
	l := slog.New(Handler(slog.NewTextHandler(&b, nil)))
	Secret("registered-secret-value")
	l.Info("got registered-secret-value", "url", "http://x/?a=1&X-Plex-Token=abcdef123&b=2",
		"err", errors.New(`Get "http://x/?token=zzz9999": refused`), "plain", "registered-secret-value")
	out := b.String()
	for _, bad := range []string{"registered-secret-value", "abcdef123", "zzz9999"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q is in the log: %s", bad, out)
		}
	}
	if !strings.Contains(out, "a=1") || !strings.Contains(out, "b=2") {
		t.Errorf("too much went: %s", out)
	}
}
