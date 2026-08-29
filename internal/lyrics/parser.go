package lyrics

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Line is one timed lyric line, matching the schema the web UI already expects
// (see fixtures/state-with-lyrics.json).
type Line struct {
	TimeMS int    `json:"time_ms"`
	Text   string `json:"text"`
}

// stampRe matches a single LRC timestamp like [01:23.45] or [01:23:456] or [01:23].
// A line can carry several stamps prefixing the same text (LRCLIB sometimes
// collapses repeated lyrics this way).
var stampRe = regexp.MustCompile(`\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]`)

// ParseLRC converts the body of LRCLIB's syncedLyrics field into a sorted
// slice of Lines. Metadata-only lines like [ar:...] and [ti:...] are skipped
// because they don't match the digit-only timestamp pattern.
func ParseLRC(body string) []Line {
	if body == "" {
		return nil
	}

	var out []Line
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimRight(raw, "\r")
		matches := stampRe.FindAllStringSubmatchIndex(raw, -1)
		if len(matches) == 0 {
			continue
		}

		textStart := matches[len(matches)-1][1]
		text := strings.TrimSpace(raw[textStart:])
		for _, m := range matches {
			mins, _ := strconv.Atoi(raw[m[2]:m[3]])
			secs, _ := strconv.Atoi(raw[m[4]:m[5]])
			frac := 0
			if m[6] >= 0 {
				fracStr := raw[m[6]:m[7]]
				n, _ := strconv.Atoi(fracStr)
				switch len(fracStr) {
				case 1:
					frac = n * 100
				case 2:
					frac = n * 10
				case 3:
					frac = n
				default:
					frac = n
				}
			}
			out = append(out, Line{
				TimeMS: mins*60000 + secs*1000 + frac,
				Text:   text,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].TimeMS < out[j].TimeMS })
	return out
}
