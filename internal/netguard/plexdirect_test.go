package netguard

import "testing"

func TestPlexDirectIP(t *testing.T) {
	for host, want := range map[string]string{
		"192-168-1-71.0123456789abcdef0123456789abcdef.plex.direct": "192.168.1.71",
		"2606-8e80-0-1--2.abc.plex.direct":                          "2606:8e80:0:1::2",
		"plex.tv":                                                   "",
		"192-168-1-2.example.com":                                   "",
		"notanip.abc.plex.direct":                                   "",
	} {
		got := PlexDirectIP(host)
		if (got == nil) != (want == "") || (got != nil && got.String() != want) {
			t.Errorf("%s: %v, want %q", host, got, want)
		}
	}
}

func TestAPlexDirectHostIsAnAddress(t *testing.T) {
	for host, want := range map[string]bool{
		"192.168.2.2:32500": true,
		"192-168-1-71.0123456789abcdef0123456789abcdef.plex.direct:32400": true,
		"evil.example.com:32500": false,
		"plex.direct":            false,
	} {
		if got := HostIsAddress(host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}
