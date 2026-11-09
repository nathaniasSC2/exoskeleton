package agent

import "testing"

func TestGovernHeartbeat(t *testing.T) {
	const now = 1794200000
	hot := `{"status":"allowed","window":"five_hour","resets_at":1794210000,"five_hour":0.8,"seven_day":0.22}`
	cases := []struct {
		name      string
		v         string
		at        int64
		skippedOn string
		want      bool
	}{
		{"hot and fresh", hot, now - 600, "", true},
		{"same reading already skipped once", hot, now - 600, "1794199400", false},
		{"a newer reading after a skip", hot, now - 60, "1794199400", true},
		{"its window has reset (the 2026-11-08 lock)", `{"window":"five_hour","resets_at":1794190000,"five_hour":0.8,"seven_day":0.22}`, now - 28800, "", false},
		{"cool", `{"window":"five_hour","resets_at":1794210000,"five_hour":0.3,"seven_day":0.5}`, now - 60, "", false},
		{"seven-day hot, no reset time", `{"window":"seven_day","five_hour":0.1,"seven_day":0.9}`, now - 60, "", true},
		{"nothing saved", "", 0, "", false},
		{"unreadable", "{", now, "", false},
	}
	for _, c := range cases {
		got, why := governHeartbeat(c.v, c.at, c.skippedOn, now)
		if got != c.want {
			t.Errorf("%s: skip = %v (%q), want %v", c.name, got, why, c.want)
		}
		if got && why == "" {
			t.Errorf("%s: skipped without a reason", c.name)
		}
	}
}
