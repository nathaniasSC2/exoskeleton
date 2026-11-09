package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/store"
)

func TestRecentHeartbeatTexts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	loc := time.UTC
	day := time.Date(2026, 11, 9, 0, 0, 0, 0, loc).Unix() // today's midnight
	add := func(person, kind string, at int64, reply string) {
		id := st.StartTurn(person, "discord", "c", kind, "in")
		st.DB.Exec(`UPDATE turns SET started=?, reply=? WHERE id=?`, at, reply, id)
	}
	add("nathan", "heartbeat", day-86400+10*3600, "MESSAGE: Doorway chest stretch, 30 seconds a side?\nLOG: move")
	add("nathan", "heartbeat", day-86400+12*3600, "MESSAGE: SILENT\nLOG: nothing new")
	add("nathan", "heartbeat", day-86400+23*3600, "MESSAGE: written in quiet hours\nLOG: x")
	add("nathan", "heartbeat", day-5*86400, "MESSAGE: too old\nLOG: x")
	add("nathan", "heartbeat", day+9*3600, "MESSAGE: today, shown elsewhere\nLOG: x")
	add("nathan", "chat", day-86400+11*3600, "a chat reply")
	add("katie", "heartbeat", day-86400+14*3600, "MESSAGE: someone else's\nLOG: x")
	add("nathan", "heartbeat", day-2*86400+14*3600, "MESSAGE: Which BlizzCon moment would you tell a stranger first?\nLOG: story")

	got := recentHeartbeatTexts(st, "nathan", day-3*86400, day, loc, 22, 7)
	want := []string{
		"- Sun 10:00AM: Doorway chest stretch, 30 seconds a side?",
		"- Sat 2:00PM: Which BlizzCon moment would you tell a stranger first?",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
