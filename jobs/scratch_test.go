package jobs

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/store"
)

func TestScratch(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := New(&config.Config{DataDir: t.TempDir(), MaxJobs: 1, JobScratch: []string{filepath.Join(dir, "j{id}")}}, st)
	mk := func(id string, age time.Duration) string {
		p := filepath.Join(dir, "j"+id)
		os.MkdirAll(filepath.Join(p, "sub"), 0o755)
		f := filepath.Join(p, "sub", "frame.png")
		os.WriteFile(f, []byte("png"), 0o644)
		ts := time.Now().Add(-age)
		os.Chtimes(f, ts, ts)
		os.Chtimes(filepath.Join(p, "sub"), ts, ts)
		os.Chtimes(p, ts, ts)
		return p
	}
	add := func(id int, status string) {
		st.DB.Exec(`INSERT INTO jobs(id,person,channel,chat,task,workdir,status,created) VALUES(?,?,?,?,?,?,?,?)`, id, "a", "d", "c", "t", "/", status, time.Now().Unix())
	}
	add(1, "done")
	add(2, "done")
	old, fresh, stranger := mk("1", 72*time.Hour), mk("2", time.Hour), mk("99", 72*time.Hour)
	got := r.StaleScratch(48 * time.Hour)
	if len(got) != 1 || got[0].Path != old || got[0].Bytes != 3 {
		t.Fatalf("stale = %+v (want only %s; %s is fresh, %s isn't a job of ours)", got, old, fresh, stranger)
	}
	add(3, "running")
	if got := r.StaleScratch(48 * time.Hour); len(got) != 0 {
		t.Fatalf("nothing is swept while a job runs: %+v", got)
	}
	r.removeScratch(2)
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("a job's scratch dir should go when it ends")
	}
	link := filepath.Join(dir, "j5")
	os.Symlink(old, link)
	r.removeScratch(5)
	if _, err := os.Stat(old); err != nil {
		t.Fatal("removeScratch followed a symlink")
	}
}

func TestTailGz(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "7.jsonl")
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"rendered the final cut"}]}}` + "\n"
	f, _ := os.Create(p + ".gz")
	zw := gzip.NewWriter(f)
	zw.Write([]byte(strings.Repeat(`{"type":"user"}`+"\n", 50000) + line))
	zw.Close()
	f.Close()
	if got := Tail(p, 3); !strings.Contains(got, "rendered the final cut") {
		t.Fatalf("Tail of a gzipped log = %q", got)
	}
}
