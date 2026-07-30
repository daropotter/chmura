package cli

import (
	"strings"

	"github.com/spf13/pflag"
)

// suggestMaxDistance is the largest edit distance at which a typo is still
// treated as a likely-intended flag. Two keeps it to real typos.
const suggestMaxDistance = 2

// unknownLongFlag extracts the flag token (e.g. "--replica") from pflag's
// "unknown flag: --x" error. It reports false for anything else, including the
// shorthand-flag error, which carries no useful long-name suggestion.
func unknownLongFlag(err error) (string, bool) {
	const prefix = "unknown flag: "
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(msg, prefix)), true
}

// suggestFlag returns the closest known long flag name to name (both without
// dashes), or "" if none is within suggestMaxDistance. An exact match is not a
// suggestion.
func suggestFlag(flags *pflag.FlagSet, name string) string {
	best := ""
	bestDist := suggestMaxDistance + 1
	flags.VisitAll(func(f *pflag.Flag) {
		if f.Name == name {
			return
		}
		if d := levenshtein(name, f.Name); d < bestDist {
			bestDist, best = d, f.Name
		}
	})
	if bestDist > suggestMaxDistance {
		return ""
	}
	return best
}

// levenshtein is the edit distance between a and b.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
