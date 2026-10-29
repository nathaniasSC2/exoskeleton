package channel

import (
	"strings"
	"testing"
)

func TestSplitBubbles(t *testing.T) {
	got := SplitBubbles("first para.\n\nsecond para.\r\n\r\nthird", 4000)
	if len(got) != 3 || got[1] != "second para." {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("word ", 1000)
	for _, b := range SplitBubbles(long, 1900) {
		if len(b) > 1900 || b == "" {
			t.Fatalf("bad chunk len %d", len(b))
		}
	}
	if joined := strings.Join(SplitBubbles(long, 1900), " "); strings.Count(joined, "word") != 1000 {
		t.Fatal("split lost words")
	}
}

func TestPackDiscord(t *testing.T) {
	got := packDiscord("a\n\nb\n\nc", 1900)
	if len(got) != 1 || got[0] != "a\n\nb\n\nc" {
		t.Fatalf("got %q", got)
	}
}
