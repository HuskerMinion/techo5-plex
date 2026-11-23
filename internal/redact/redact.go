// Package redact keeps Plex tokens out of the log. Every token the add-on handles is registered with
// Secret, and the log handler replaces each registered value, and anything that looks like a token in
// a query string, before a line is written. The callers already avoid logging URLs that carry a token;
// this is the net under them.
package redact

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
)

var (
	mu      sync.RWMutex
	secrets = map[string]struct{}{}
)

// Secret registers a value that must never be written to the log. Short values are ignored: they
// would replace ordinary words.
func Secret(s string) {
	if len(s) < 8 {
		return
	}
	mu.Lock()
	secrets[s] = struct{}{}
	mu.Unlock()
}

// queryToken is a token in a query string or a header-like dump: X-Plex-Token=..., token=...,
// X-Plex-Token: ...
var queryToken = regexp.MustCompile(`(?i)((?:x-plex-token|token|authToken)["']?\s*[=:]\s*["']?)[^&\s"'<>]+`)

// String is s with every secret and token-looking value replaced.
func String(s string) string {
	mu.RLock()
	for sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, "<token>")
		}
	}
	mu.RUnlock()
	return queryToken.ReplaceAllString(s, "${1}<token>")
}

// Handler wraps h so every attribute and message is passed through String.
func Handler(h slog.Handler) slog.Handler { return handler{h} }

type handler struct{ h slog.Handler }

func (r handler) Enabled(ctx context.Context, l slog.Level) bool { return r.h.Enabled(ctx, l) }

func (r handler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(attr(a))
		return true
	})
	return r.h.Handle(ctx, out)
}

func (r handler) WithAttrs(as []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(as))
	for i, a := range as {
		clean[i] = attr(a)
	}
	return handler{r.h.WithAttrs(clean)}
}

func (r handler) WithGroup(name string) slog.Handler { return handler{r.h.WithGroup(name)} }

func attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(v.String()))
	case slog.KindGroup:
		g := v.Group()
		clean := make([]any, len(g))
		for i, ga := range g {
			clean[i] = attr(ga)
		}
		return slog.Group(a.Key, clean...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, String(x.Error()))
		case interface{ String() string }:
			return slog.String(a.Key, String(x.String()))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}
