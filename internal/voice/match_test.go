package voice

import (
	"testing"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/companion"
)

func lib(titles ...any) []*Entry {
	var out []*Entry
	for i := 0; i+2 < len(titles); i += 3 {
		e := &Entry{Kind: titles[i].(string), Title: titles[i+1].(string), Year: titles[i+2].(int)}
		e.RatingKey = e.Title
		e.norm = titleKey(e.Title)
		out = append(out, e)
	}
	return out
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"The Office":             "office",
		"the office (US)":        "office us",
		"Ocean's Eleven":         "oceans 11",
		"Law & Order: SVU":       "law and order svu",
		"Amélie":                 "amelie",
		"Rocky II":               "rocky 2",
		"I, Robot":               "i robot",
		"The":                    "the",
		"  WALL·E  ":             "wall e",
		"Star Wars: Episode IV":  "star wars episode 4",
		"fifty first dates":      "50 first dates",
		"50 First Dates":         "50 first dates",
		"twenty-one jump street": "21 jump street",
		"Ocean's Twenty":         "oceans 20",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := titleKey("Doctor Who (2005)"); got != "doctor who" {
		t.Errorf("titleKey = %q", got)
	}
	// A title that is a year keeps it.
	if got := titleKey("(2012)"); got != "2012" {
		t.Errorf("titleKey of a year = %q", got)
	}
}

func TestFind(t *testing.T) {
	entries := lib(
		KindShow, "The Office (US)", 2005,
		KindShow, "Doctor Who (2005)", 2005,
		KindShow, "Doctor Who", 1963,
		KindMovie, "Ocean's Eleven", 2001,
		KindMovie, "Rocky II", 1979,
		KindMovie, "Rocky", 1976,
		KindMovie, "2012", 2009,
		KindMovie, "Blade Runner", 1982,
		KindMovie, "Blade Runner 2049", 2017,
		KindShow, "Star Trek: Picard", 2020,
		KindMovie, "Amélie", 2001,
		KindMovie, "Fargo", 1996,
		KindShow, "Fargo", 2014,
	)
	for said, want := range map[string]string{
		"the office us":     "The Office (US)",
		"office":            "The Office (US)", // the start of the title
		"the ofice":         "The Office (US)", // speech to text slightly wrong
		"doctor who":        "Doctor Who (2005)",
		"doctor who 1963":   "Doctor Who",
		"oceans eleven":     "Ocean's Eleven",
		"ocean's 11":        "Ocean's Eleven",
		"rocky two":         "Rocky II",
		"rocky":             "Rocky",
		"2012":              "2012",
		"blade runner 2049": "Blade Runner 2049",
		"blade runner":      "Blade Runner",
		"star trek":         "Star Trek: Picard",
		"amelie":            "Amélie",
		"Fargo (1996)":      "Fargo",
	} {
		m := find(entries, said)
		if m == nil {
			t.Errorf("%q: nothing", said)
			continue
		}
		if m.Entry.Title != want {
			t.Errorf("%q: %q (%d), want %q", said, m.Entry.Title, m.Entry.Year, want)
		}
	}
	for _, said := range []string{"zardoz", "", "the", "office space the movie"} {
		if m := find(entries, said); m != nil {
			t.Errorf("%q: %q", said, m.Entry.Title)
		}
	}
	// Only shows, for an episode.
	if m := find(entries, "fargo", KindShow); m == nil || m.Entry.Kind != KindShow {
		t.Error("fargo the show not found among shows")
	}
	if m := find(entries, "rocky", KindShow); m != nil {
		t.Errorf("a show found: %q", m.Entry.Title)
	}
}

func TestName(t *testing.T) {
	entries := lib(KindMovie, "Dune", 1984, KindMovie, "Dune", 2021, KindMovie, "Heat", 1995,
		KindMovie, "Shogun", 1980, KindShow, "Shogun", 1980)
	for said, want := range map[string]string{
		"dune":      "Dune (2021)",
		"dune 1984": "Dune (1984)",
		"heat":      "Heat",
		"shogun":    "Shogun, the movie", // one of each, of the same year: the first listed
	} {
		m := find(entries, said)
		if m == nil {
			t.Fatalf("%q: nothing", said)
		}
		if got := name(m); got != want {
			t.Errorf("%q: %q, want %q", said, got, want)
		}
	}
}

func TestNextUp(t *testing.T) {
	ep := func(s, e int, watched bool, offset time.Duration) companion.Item {
		return companion.Item{Season: s, Episode: e, Watched: watched, ViewOffset: offset}
	}
	for name, c := range map[string]struct {
		eps  []companion.Item
		s, e int
	}{
		"none watched":      {[]companion.Item{ep(1, 1, false, 0), ep(1, 2, false, 0)}, 1, 1},
		"after the last":    {[]companion.Item{ep(1, 1, false, 0), ep(1, 2, true, 0), ep(1, 3, false, 0)}, 1, 3},
		"one skipped, last": {[]companion.Item{ep(1, 1, false, 0), ep(1, 2, true, 0)}, 1, 1},
		"started":           {[]companion.Item{ep(1, 1, true, 0), ep(1, 2, false, 0), ep(1, 3, false, time.Minute)}, 1, 3},
		"all watched":       {[]companion.Item{ep(1, 1, true, 0), ep(1, 2, true, 0)}, 1, 1},
		"specials aside":    {[]companion.Item{ep(0, 1, false, 0), ep(1, 1, true, 0), ep(1, 2, false, 0)}, 1, 2},
		"specials only":     {[]companion.Item{ep(0, 1, true, 0), ep(0, 2, false, 0)}, 0, 2},
	} {
		got, ok := nextUp(c.eps)
		if !ok || got.Season != c.s || got.Episode != c.e {
			t.Errorf("%s: S%dE%d, want S%dE%d", name, got.Season, got.Episode, c.s, c.e)
		}
	}
	if _, ok := nextUp(nil); ok {
		t.Error("an episode of none")
	}
}

func TestNumber(t *testing.T) {
	for in, want := range map[string]int{"4": 4, " 12 ": 12, "four": 4, "Twelve": 12, "3rd": 3, "1st": 1, "iv": 4, "twenty one": 21, "Twenty-One": 21, "fifty": 50} {
		if got, ok := number(in); !ok || got != want {
			t.Errorf("number(%q) = %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "x", "-1", "a lot", "99999", "twenty two three"} {
		if _, ok := number(in); ok {
			t.Errorf("number(%q) taken", in)
		}
	}
}

func TestSpoken(t *testing.T) {
	for d, want := range map[time.Duration]string{
		62*time.Minute + 20*time.Second: "1 hour 2 minutes",
		2 * time.Hour:                   "2 hours",
		time.Hour + time.Minute:         "1 hour 1 minute",
		45 * time.Minute:                "45 minutes",
		30 * time.Second:                "30 seconds",
		time.Second:                     "1 second",
		200 * time.Millisecond:          "1 second",
	} {
		if got := spoken(d); got != want {
			t.Errorf("spoken(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFromStart(t *testing.T) {
	for _, c := range []struct {
		title string
		args  map[string]any
		want  string
		over  bool
	}{
		{"dune", nil, "dune", false},
		{"dune from the beginning", nil, "dune", true},
		{"Dune From The Start", nil, "Dune", true},
		{"game over", nil, "game over", false},
		{"from the beginning", nil, "from the beginning", false},
		{"dune", map[string]any{"from_start": true}, "dune", true},
		{"dune", map[string]any{"from_start": "yes"}, "dune", true},
		{"dune", map[string]any{"from_start": 1.0}, "dune", true},
		{"dune", map[string]any{"from_start": false}, "dune", false},
	} {
		got, over := fromStart(c.title, c.args)
		if got != c.want || over != c.over {
			t.Errorf("fromStart(%q, %v) = %q %v", c.title, c.args, got, over)
		}
	}
}

func TestChoiceOf(t *testing.T) {
	for said, want := range map[string]int{ // 1 resume, 0 start over, -1 neither
		"resume": 1, "Resume.": 1, "continue": 1, "keep going": 1, "where I left off": 1, "yes": 1,
		"and resume please": 1, "um, continue": 1, "uh where i left off": 1,
		"from the beginning": 0, "the start": 0, "start over": 0, "start it over": 0, "restart": 0,
		"from the start": 0, "no": 0, "and, um, the beginning please": 0, "from the top": 0,
		"": -1, "banana": -1, "um": -1, "the": -1,
	} {
		resume, ok := choiceOf(said)
		got := -1
		if ok {
			got = 0
			if resume {
				got = 1
			}
		}
		if got != want {
			t.Errorf("choiceOf(%q) = %d, want %d", said, got, want)
		}
	}
}
