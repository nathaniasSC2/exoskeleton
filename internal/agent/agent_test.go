package agent

import "testing"

func TestSplitHeartbeat(t *testing.T) {
	cases := []struct{ in, msg, log string }{
		{"MESSAGE: SILENT\nLOG: checked lions, nothing new", "", "checked lions, nothing new"},
		{"MESSAGE: Sadiq is cleared for Sunday 🟢\nLOG: injury report updated", "Sadiq is cleared for Sunday 🟢", "injury report updated"},
		{"MESSAGE: silent.\nLOG: x", "", "x"},
		{"SILENT", "", ""},
	}
	for _, c := range cases {
		m, l := splitHeartbeat(c.in)
		if m != c.msg || l != c.log {
			t.Errorf("splitHeartbeat(%q) = %q, %q", c.in, m, l)
		}
	}
}

func TestInQuiet(t *testing.T) {
	for h, want := range map[int]bool{21: false, 22: true, 23: true, 0: true, 5: true, 6: false, 12: false} {
		if inQuiet(h, 22, 6) != want {
			t.Errorf("inQuiet(%d, 22, 6) = %v", h, !want)
		}
	}
	if inQuiet(3, 6, 6) {
		t.Error("equal bounds should mean no quiet hours")
	}
}

func TestKeywords(t *testing.T) {
	got := keywords("hey exo what's the latest status on gardenlog and the jellyfin server?")
	want := map[string]bool{"gardenlog": true, "jellyfin": true, "server": true}
	if len(got) != len(want) {
		t.Fatalf("keywords = %v", got)
	}
	for _, w := range got {
		if !want[w] {
			t.Fatalf("unexpected keyword %q in %v", w, got)
		}
	}
	if k := keywords("yo how's it going lol"); len(k) != 0 {
		t.Fatalf("small talk produced keywords %v", k)
	}
}
