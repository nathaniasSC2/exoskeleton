package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	st, err := Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB.Close() })
	return st
}

func TestEventDedupe(t *testing.T) {
	st := open(t)
	e := &Event{Channel: "discord", Chat: "c", Sender: "s", Person: "alex", GUID: "g1", Text: "hi", Images: "[]", TS: time.Now().Unix()}
	if ok, err := st.AddEvent(e); !ok || err != nil {
		t.Fatalf("first insert: %v %v", ok, err)
	}
	if ok, _ := st.AddEvent(&Event{Channel: "discord", Chat: "c", Sender: "s", Person: "alex", GUID: "g1", Text: "hi", Images: "[]", TS: time.Now().Unix()}); ok {
		t.Fatal("duplicate GUID was accepted")
	}
	pend, _ := st.PendingEvents("alex", time.Hour)
	if len(pend) != 1 {
		t.Fatalf("pending = %d", len(pend))
	}
	st.MarkEvents([]int64{pend[0].ID}, "done", 1)
	if pend, _ = st.PendingEvents("alex", time.Hour); len(pend) != 0 {
		t.Fatal("marked event still pending")
	}
}

func TestRecentEpochAndFTS(t *testing.T) {
	st := open(t)
	for i, in := range []string{"how about the lions game", "make me a teriyaki recipe", "screenshot example.com"} {
		id := st.StartTurn("alex", "discord", "c", "chat", in)
		st.FinishTurn(&Turn{ID: id, Status: "ok", Reply: "reply " + string(rune('a'+i))})
	}
	st.StartTurn("alex", "discord", "c", "chat", "unfinished") // running: must not show in Recent
	rec, _ := st.Recent("alex", 10)
	if len(rec) != 3 || rec[0].Input != "how about the lions game" {
		t.Fatalf("recent = %d, first %q", len(rec), rec[0].Input)
	}
	st.Set("epoch:alex", "2")
	if rec, _ = st.Recent("alex", 10); len(rec) != 1 {
		t.Fatalf("after epoch recent = %d", len(rec))
	}
	hits, _ := st.SearchTurns("alex", "what did we say about teriyaki?", 5)
	if len(hits) != 1 || !strings.Contains(hits[0].Input, "teriyaki") {
		t.Fatalf("fts hits = %+v", hits)
	}
	if hits, _ = st.SearchTurns("sam", "teriyaki", 5); len(hits) != 0 {
		t.Fatal("search leaked across people")
	}
	n := st.AddNotice("alex", "discord", "c", "notice", "Job #3 done: rebuilt gardenlog")
	if hits, _ = st.SearchTurns("alex", "gardenlog", 5); len(hits) != 1 || hits[0].ID != n {
		t.Fatal("notice not searchable")
	}
}

func TestFactsAndGUIDs(t *testing.T) {
	st := open(t)
	st.AddFact("alex", "Alex is a Lions fan", true, "")
	st.AddFact("alex", "Lou is their golden retriever", false, "")
	if id, _ := st.AddFact("alex", "Lou is their golden retriever", false, ""); id == 0 {
		t.Fatal("dup fact returned 0")
	}
	all, _ := st.Facts("alex", false)
	if len(all) != 2 {
		t.Fatalf("facts = %d (dup inserted?)", len(all))
	}
	hits, _ := st.SearchFacts("alex", "how's lou doing", 5)
	if len(hits) != 1 {
		t.Fatalf("fact hits = %d", len(hits))
	}
	if hits, _ = st.SearchFacts("alex", "lions", 5); len(hits) != 0 {
		t.Fatal("pinned fact returned by search (it's already in the prompt)")
	}
	id := st.StartTurn("alex", "discord", "c", "chat", "x")
	st.AddOutGUID(id, "AAA-1")
	st.AddOutGUID(id, "BBB-2")
	if st.TurnByOutGUID("BBB-2") != id || st.TurnByOutGUID("BBB") != 0 {
		t.Fatal("out guid lookup wrong")
	}
}

func TestFTSQuerySanitizes(t *testing.T) {
	if q := ftsQuery(`"NEAR(" OR * AND -- the lions?`); strings.ContainsAny(q, "*-(") || !strings.Contains(q, `"lions"`) {
		t.Fatalf("ftsQuery = %q", q)
	}
	if ftsQuery("yo lol ok") != "" {
		t.Fatal("stopwords produced a query")
	}
}
