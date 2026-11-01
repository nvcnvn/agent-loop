package memory

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func FuzzSplit(f *testing.F) {
	f.Add("one\n\ntwo paragraphs", 8)
	f.Add("a word that is far too long for one part", 5)
	f.Add("ừừừừừ", 2)
	f.Add("\n\n\n\n", 1)
	f.Fuzz(func(t *testing.T, text string, max int) {
		if max < 1 || max > 1<<12 || !utf8.ValidString(text) {
			return
		}
		var kept strings.Builder
		for _, part := range split(text, max) {
			if part == "" || len(part) > max && len([]rune(part)) > 1 {
				t.Fatalf("part %q of %d bytes, max %d", part, len(part), max)
			}
			kept.WriteString(part)
		}
		if strip(kept.String()) != strip(text) {
			t.Fatalf("split lost text: %q from %q", kept.String(), text)
		}
	})
}

func strip(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
