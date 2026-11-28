package sched

import (
	"context"
	"testing"
	"time"
)

func TestUntil(t *testing.T) {
	loc := time.FixedZone("PT", -8*3600)
	e := Entry{Name: "push", Until: "2026-11-26"}
	end, ok := e.UntilTime(loc)
	if !ok || !end.Equal(time.Date(2026, 11, 27, 0, 0, 0, 0, loc)) {
		t.Fatalf("until a day means through its end: %v %v", end, ok)
	}
	if e.Expired(time.Date(2026, 11, 26, 23, 59, 0, 0, loc), loc) || !e.Expired(time.Date(2026, 11, 27, 0, 0, 0, 0, loc), loc) {
		t.Fatal("expiry boundary")
	}
	if (Entry{}).Expired(time.Now(), loc) {
		t.Fatal("no until never expires")
	}
	if _, err := ParseUntil("next tuesday", loc); err == nil {
		t.Fatal("garbage parsed")
	}
	if (Entry{Until: "next tuesday"}).Expired(time.Now(), loc) {
		t.Fatal("an unreadable until must not stop the entry")
	}
	if !Manual(context.WithValue(context.Background(), manualKey{}, true)) || Manual(context.Background()) {
		t.Fatal("manual marker")
	}
}
