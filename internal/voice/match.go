package voice

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Matching what was said against the library's titles. The lessons are Stoney's (stoney66, who voiced
// Plex through Home Assistant scripts made from his library before this add-on did it itself): speech
// gives lowercase words without punctuation, a leading "the" comes and goes, and a title with its year
// in it, "Doctor Who (2005)", never matched what anybody says. So both sides are brought to the same
// plain words first, and then compared loosely enough for what speech to text gets slightly wrong.

// Kinds of entry in the library.
const (
	KindMovie = "movie"
	KindShow  = "show"
)

// accept is the least score a title needs to be taken for what was said.
const accept = 0.72

// tieWithin is how close two scores are to be the same match, decided by the year instead.
const tieWithin = 0.02

// parenYear is a year in brackets at the end of a title: "Doctor Who (2005)".
var parenYear = regexp.MustCompile(`\s*[\(\[]\s*(1[89]\d\d|20\d\d)\s*[\)\]]\s*$`)

// spokenYear is a year at the end of what was said, bare or in brackets: "dune 2021".
var spokenYear = regexp.MustCompile(`\s*[\(\[]?\s*\b(1[89]\d\d|20\d\d)\s*[\)\]]?\s*$`)

// numbers are the words speech or a title may spell a small number in, as digits.
var numbers = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7",
	"eight": "8", "nine": "9", "ten": "10", "eleven": "11", "twelve": "12", "thirteen": "13",
	"fourteen": "14", "fifteen": "15", "sixteen": "16", "seventeen": "17", "eighteen": "18",
	"nineteen": "19", "twenty": "20", "thirty": "30", "forty": "40", "fifty": "50", "sixty": "60",
	"seventy": "70", "eighty": "80", "ninety": "90",
	// Roman numerals of sequels, but not "i", which is a word.
	"ii": "2", "iii": "3", "iv": "4", "vi": "6", "vii": "7", "viii": "8", "ix": "9",
}

// folds are the accented letters titles have most, as plain ones.
var folds = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a", "æ", "ae",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o", "ø", "o", "œ", "oe",
	"ú", "u", "ù", "u", "û", "u", "ü", "u",
	"ñ", "n", "ç", "c", "ß", "ss", "ý", "y", "ÿ", "y",
)

// normalize is a title as plain words: lowercase, no accents, no punctuation, "&" as "and", numbers
// as digits, and no leading "the".
func normalize(s string) string {
	s = folds.Replace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "&", " and ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\'' || r == '’' || r == '‘' || r == '`':
			// "Ocean's" is "oceans": an apostrophe joins, it does not part.
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	words := strings.Fields(b.String())
	spelled := make([]bool, len(words))
	for i, w := range words {
		if d, ok := numbers[w]; ok {
			words[i], spelled[i] = d, true
		}
	}
	// "twenty one" (or "twenty-one") is 21: a ten spelled out before a digit spelled out is one number.
	out := words[:0]
	for i := 0; i < len(words); i++ {
		w := words[i]
		if i+1 < len(words) && spelled[i] && spelled[i+1] && len(w) == 2 && w[1] == '0' && w[0] >= '2' &&
			len(words[i+1]) == 1 && words[i+1] != "0" {
			w = w[:1] + words[i+1]
			i++
		}
		out = append(out, w)
	}
	words = out
	if len(words) > 1 && words[0] == "the" {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// titleKey is a library title as it is matched: without a year in brackets at its end.
func titleKey(title string) string {
	if t := parenYear.ReplaceAllString(title, ""); strings.TrimSpace(t) != "" {
		title = t
	}
	return normalize(title)
}

// query is what was said, as it is matched: whole, and without a year at its end (with the year).
type query struct {
	full     string
	bare     string
	year     int
	original string
}

func parseQuery(said string) query {
	q := query{original: strings.TrimSpace(said), full: normalize(parenYear.ReplaceAllString(said, ""))}
	if m := spokenYear.FindStringSubmatch(said); m != nil {
		if rest := strings.TrimSpace(said[:len(said)-len(m[0])]); normalize(rest) != "" {
			q.bare = normalize(rest)
			q.year, _ = strconv.Atoi(m[1])
		}
	}
	if q.bare == "" {
		q.bare = q.full
	}
	return q
}

// score is how well a query fits an entry, 0 to 1.
func (q query) score(e *Entry) float64 {
	s := similarity(q.full, e.norm)
	if q.year != 0 {
		b := similarity(q.bare, e.norm)
		if e.Year == q.year {
			b = min(1, b+0.05) // "dune 2021" is the 2021 Dune
		} else {
			b -= 0.1 // the year said is not this one's, but the title may be
		}
		s = max(s, b)
	}
	return s
}

// similarity is how alike two normalized titles are: the same, one the start of the other, nearly
// the same letters, or nearly the same words.
func similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	best := letters(a, b)
	// What was said is the start of the title, or nearly: "star trek" for "Star Trek: Picard", "the
	// ofice" for "The Office (US)".
	wa, wb := strings.Fields(a), strings.Fields(b)
	if len(a) >= 4 && len(wb) > len(wa) {
		start := strings.Join(wb[:len(wa)], " ")
		best = max(best, letters(a, start)*(0.8+0.15*float64(len(a))/float64(len(b))))
	}
	return max(best, words(a, b))
}

// letters is 1 less the edit distance over the longer length.
func letters(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	n := max(len(ra), len(rb))
	return 1 - float64(distance(ra, rb))/float64(n)
}

// words is the share of words the two have alike, a word near enough another counting as it.
func words(a, b string) float64 {
	wa, wb := strings.Fields(a), strings.Fields(b)
	used := make([]bool, len(wb))
	var alike float64
	for _, x := range wa {
		at, best := -1, 0.0
		for j, y := range wb {
			if used[j] {
				continue
			}
			if s := letters(x, y); s > best {
				at, best = j, s
			}
		}
		if at >= 0 && best >= 0.8 {
			used[at] = true
			alike += best
		}
	}
	return 2 * alike / float64(len(wa)+len(wb))
}

// distance is the Levenshtein distance.
func distance(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// Match is the entry taken for what was said, and the others of the same title, which the answer tells
// it apart from (by its year, or by its being a show or a film).
type Match struct {
	Entry *Entry
	Score float64
	Ties  []*Entry
}

// find is the best entry for what was said among entries of the kinds given (all for none), or nil.
func find(entries []*Entry, said string, kinds ...string) *Match {
	q := parseQuery(said)
	if q.full == "" {
		return nil
	}
	type scored struct {
		e *Entry
		s float64
	}
	var all []scored
	best := 0.0
	for _, e := range entries {
		if len(kinds) > 0 && !slices.Contains(kinds, e.Kind) {
			continue
		}
		s := q.score(e)
		if s < accept {
			continue
		}
		all = append(all, scored{e, s})
		best = max(best, s)
	}
	if len(all) == 0 {
		return nil
	}
	var top []*Entry
	for _, x := range all {
		if x.s >= best-tieWithin {
			top = append(top, x.e)
		}
	}
	// Among the best: the one whose year was said, then one whose title is exactly what was said,
	// then the newest.
	slices.SortStableFunc(top, func(a, b *Entry) int {
		if ya, yb := q.year != 0 && a.Year == q.year, q.year != 0 && b.Year == q.year; ya != yb {
			if ya {
				return -1
			}
			return 1
		}
		if ea, eb := a.norm == q.full || a.norm == q.bare, b.norm == q.full || b.norm == q.bare; ea != eb {
			if ea {
				return -1
			}
			return 1
		}
		return b.Year - a.Year
	})
	m := &Match{Entry: top[0], Score: best}
	for _, x := range all {
		if x.e != m.Entry && x.e.norm == m.Entry.norm {
			m.Ties = append(m.Ties, x.e)
		}
	}
	return m
}
