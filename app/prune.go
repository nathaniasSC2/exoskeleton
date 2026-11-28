package app

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/store"
)

// Retention is how long the harness keeps what it writes. Before it, nightly
// maintenance pruned only traces, events and runs (30 days) and inbox/outbox
// (14): job transcripts reached 1.1 GB in seven weeks, 910 MB of it in one
// week, and job scratch dirs 6 GB of /tmp (2026-11-27 audit).
type Retention struct {
	TraceDays     int           // trace rows, inbound events, scheduled runs
	ShadowDays    int           // shadow-mode rows
	MediaDays     int           // inbox/outbox files
	JobGzipDays   int           // job transcripts are gzipped this long after the job ended
	JobDeleteDays int           // and a job's files deleted this long after
	JobCap        int64         // the jobs dir's size cap: the oldest ended jobs' files go first
	ScratchAge    time.Duration // leftover job scratch dirs, swept only while no job runs
	KeepBaks      int           // .bak copies kept per file (schedule.json, each prompt)
}

var DefaultRetention = Retention{TraceDays: 30, ShadowDays: 30, MediaDays: 14, JobGzipDays: 7, JobDeleteDays: 30,
	JobCap: 2 << 30, ScratchAge: 48 * time.Hour, KeepBaks: 3}

// Removal is one thing pruning removes or compresses.
type Removal struct {
	Kind   string // what it belongs to: "job 141", "scratch", ".bak", "inbox", a KeepNewest label, "rows"
	Action string // delete | gzip | rows
	Path   string // the file or directory; for rows, the table (and kv prefix)
	Size   int64  // bytes on disk (before gzip); for rows, the row count
	Saves  int64  // bytes it frees (a gzip's is estimated in a dry run)
}

type keepRule struct {
	glob, label string
	n           int
}

// PruneKV has nightly maintenance delete kv keys starting with prefix once
// they're days old (per-item markers a deployment writes, say). Call before Run.
func (ap *App) PruneKV(prefix string, days int) {
	if ap.pruneKV == nil {
		ap.pruneKV = map[string]int{}
	}
	ap.pruneKV[prefix] = days
}

// KeepNewest has nightly maintenance keep only the newest n files matching
// glob (inside the data dir), by modification time. Call before Run.
func (ap *App) KeepNewest(glob string, n int, label string) {
	ap.keep = append(ap.keep, keepRule{glob, label, n})
}

// Prune applies the retention (with dry, only says what it would do). It
// returns every removal and every problem; nothing fails silently.
func (ap *App) Prune(ctx context.Context, dry bool) ([]Removal, []string) {
	r, cfg := ap.Retention, ap.Cfg
	var out []Removal
	var problems []string
	rows, err := ap.Store.Prune(ctx, store.PruneOpts{TraceDays: r.TraceDays, ShadowDays: r.ShadowDays, KV: ap.pruneKV, Dry: dry})
	if err != nil {
		problems = append(problems, "database prune: "+err.Error())
	}
	names := make([]string, 0, len(rows))
	for k := range rows {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if rows[k] > 0 {
			out = append(out, Removal{Kind: "rows", Action: "rows", Path: k, Size: rows[k]})
		}
	}

	apply := func(rs []Removal) {
		out = append(out, rs...)
		if dry {
			return
		}
		for _, x := range rs {
			if err := ap.applyRemoval(x); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	jobs := ap.jobFiles()
	apply(jobs.byAge(r, time.Now()))
	apply(jobs.overCap(r.JobCap, out, dry))
	if ap.Jobs != nil {
		var rs []Removal
		for _, s := range ap.Jobs.StaleScratch(r.ScratchAge) {
			rs = append(rs, Removal{Kind: fmt.Sprintf("scratch (job %d, last touched %s)", s.Job, s.Newest.Format("Jan 2")), Action: "delete", Path: s.Path, Size: s.Bytes, Saves: s.Bytes})
		}
		apply(rs)
	}
	for _, dir := range []string{"inbox", "outbox"} {
		var rs []Removal
		ents, _ := os.ReadDir(cfg.Path(dir))
		for _, e := range ents {
			if info, err := e.Info(); err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > time.Duration(r.MediaDays)*24*time.Hour {
				rs = append(rs, Removal{Kind: dir, Action: "delete", Path: cfg.Path(dir, e.Name()), Size: info.Size(), Saves: info.Size()})
			}
		}
		apply(rs)
	}
	apply(keepNewest(cfg.Path("schedule.json.bak*"), r.KeepBaks, ".bak"))
	prompts, _ := filepath.Glob(cfg.Path("prompts", "*.md"))
	for _, p := range prompts {
		apply(keepNewest(p+".bak*", r.KeepBaks, ".bak"))
	}
	for _, k := range ap.keep {
		if rel, err := filepath.Rel(cfg.DataDir, k.glob); err != nil || strings.HasPrefix(rel, "..") {
			problems = append(problems, "keep rule outside the data dir, skipped: "+k.glob)
			continue
		}
		apply(keepNewest(k.glob, k.n, k.label))
	}
	return out, problems
}

// keepNewest lists the files matching glob beyond the newest n.
func keepNewest(glob string, n int, label string) []Removal {
	paths, _ := filepath.Glob(glob)
	type f struct {
		path string
		info os.FileInfo
	}
	var fs []f
	for _, p := range paths {
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() {
			fs = append(fs, f{p, info})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].info.ModTime().After(fs[j].info.ModTime()) })
	var out []Removal
	for i := n; i < len(fs); i++ {
		out = append(out, Removal{Kind: label, Action: "delete", Path: fs[i].path, Size: fs[i].info.Size(), Saves: fs[i].info.Size()})
	}
	return out
}

func (ap *App) applyRemoval(x Removal) error {
	switch x.Action {
	case "gzip":
		if err := gzipFile(x.Path); err != nil {
			return fmt.Errorf("gzip %s: %v", x.Path, err)
		}
	case "delete":
		if !ap.removable(x.Path) {
			return fmt.Errorf("refused to delete %s: outside the data dir and the job scratch dirs", x.Path)
		}
		if err := os.RemoveAll(x.Path); err != nil {
			return fmt.Errorf("delete %s: %v", x.Path, err)
		}
	}
	return nil
}

// removable: pruning only ever deletes inside the data dir (never the data
// dir itself, the database or its WAL), or a job scratch dir.
func (ap *App) removable(path string) bool {
	path = filepath.Clean(path)
	if rel, err := filepath.Rel(ap.Cfg.DataDir, path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return !strings.HasPrefix(filepath.Base(path), ap.Cfg.Slug()+".db")
	}
	for _, pat := range ap.Cfg.JobScratch {
		re := regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(pat), regexp.QuoteMeta("{id}"), `[0-9]+`) + "$")
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// gzipFile replaces path with path.gz (same mode and modification time).
func gzipFile(path string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := path + ".gz.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if serr := out.Sync(); err == nil {
		err = serr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		os.Chtimes(tmp, info.ModTime(), info.ModTime())
		err = os.Rename(tmp, path+".gz")
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(path)
}

// ---- job files ----

var jobFileRe = regexp.MustCompile(`^([0-9]+)\.(jsonl|err|prompt|brief|mcp\.json)(\.gz)?$`)

type jobSet struct {
	dir  string
	jobs map[int64]*jobEntry
}

type jobEntry struct {
	id     int64
	ended  time.Time // zero while queued or running
	orphan bool      // no row in the jobs table
	files  []jobFile
}

type jobFile struct {
	path string
	size int64
	gz   bool
	kind string // jsonl | err | prompt | brief | mcp.json
}

// jobFiles groups the jobs dir by job and reads when each job ended. A file
// whose job isn't in the database counts as ended when it was last written.
func (ap *App) jobFiles() *jobSet {
	js := &jobSet{dir: ap.Cfg.Path("jobs"), jobs: map[int64]*jobEntry{}}
	ents, _ := os.ReadDir(js.dir)
	for _, e := range ents {
		m := jobFileRe.FindStringSubmatch(e.Name())
		info, err := e.Info()
		if m == nil || err != nil || !info.Mode().IsRegular() {
			continue
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		j := js.jobs[id]
		if j == nil {
			j = &jobEntry{id: id}
			var status string
			var ended int64
			if ap.Store.DB.QueryRow(`SELECT status, COALESCE(finished, created) FROM jobs WHERE id=?`, id).Scan(&status, &ended) != nil {
				j.orphan = true
			} else if status != "queued" && status != "running" {
				j.ended = time.Unix(ended, 0)
			}
			js.jobs[id] = j
		}
		j.files = append(j.files, jobFile{path: filepath.Join(js.dir, e.Name()), size: info.Size(), gz: m[3] != "", kind: m[2]})
		if j.orphan && info.ModTime().After(j.ended) {
			j.ended = info.ModTime() // not in the database: its newest file says when it ended
		}
	}
	return js
}

func (js *jobSet) sorted() []*jobEntry {
	var out []*jobEntry
	for _, j := range js.jobs {
		if !j.ended.IsZero() {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ended.Before(out[b].ended) })
	return out
}

// byAge: a job's files go JobDeleteDays after it ended; its transcript and
// error log (over 4 KB) are gzipped after JobGzipDays.
func (js *jobSet) byAge(r Retention, now time.Time) []Removal {
	var out []Removal
	for _, j := range js.sorted() {
		age := now.Sub(j.ended)
		kind := fmt.Sprintf("job %d (ended %s)", j.id, j.ended.Format("Jan 2"))
		switch {
		case r.JobDeleteDays > 0 && age > time.Duration(r.JobDeleteDays)*24*time.Hour:
			for _, f := range j.files {
				out = append(out, Removal{Kind: kind, Action: "delete", Path: f.path, Size: f.size, Saves: f.size})
			}
		case r.JobGzipDays > 0 && age > time.Duration(r.JobGzipDays)*24*time.Hour:
			for _, f := range j.files {
				if !f.gz && (f.kind == "jsonl" || f.kind == "err") && f.size > 4096 {
					out = append(out, Removal{Kind: kind, Action: "gzip", Path: f.path, Size: f.size, Saves: f.size * 85 / 100})
				}
			}
		}
	}
	return out
}

// overCap: while the jobs dir is over cap, the oldest ended jobs' files go,
// whole. done is what earlier steps removed or compressed: in a dry run the
// files are still there, so their (estimated) savings are counted here.
func (js *jobSet) overCap(cap int64, done []Removal, dry bool) []Removal {
	if cap <= 0 {
		return nil
	}
	gone := map[string]bool{}
	var total int64
	for _, x := range done {
		if x.Action == "delete" || x.Action == "gzip" {
			gone[x.Path] = x.Action == "delete"
			if dry {
				total -= x.Saves
			}
		}
	}
	// Measure what's there now (after a real run's earlier steps).
	ents, _ := os.ReadDir(js.dir)
	for _, e := range ents {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	var out []Removal
	for _, j := range js.sorted() {
		if total <= cap {
			break
		}
		kind := fmt.Sprintf("job %d (ended %s; jobs dir over %s)", j.id, j.ended.Format("Jan 2"), human(cap))
		for _, f := range j.files {
			if del, seen := gone[f.path]; seen && del {
				continue
			}
			size := f.size
			path := f.path
			if !dry && !f.gz {
				if st, err := os.Stat(f.path + ".gz"); err == nil { // compressed a moment ago
					path, size = f.path+".gz", st.Size()
				}
			}
			out = append(out, Removal{Kind: kind, Action: "delete", Path: path, Size: size, Saves: size})
			total -= size
		}
	}
	return out
}

// ---- the report ----

// FormatPrune is a plain-text report of a prune: totals per kind of thing,
// then (with all) every path.
func FormatPrune(rs []Removal, problems []string, all bool) string {
	var b strings.Builder
	var saves int64
	type agg struct {
		n          int
		size, save int64
	}
	groups := map[string]*agg{}
	var order []string
	for _, x := range rs {
		if x.Action == "rows" {
			fmt.Fprintf(&b, "database: %d %s rows\n", x.Size, x.Path)
			continue
		}
		g := x.Action + " " + group(x.Kind)
		if groups[g] == nil {
			groups[g] = &agg{}
			order = append(order, g)
		}
		groups[g].n++
		groups[g].size += x.Size
		groups[g].save += x.Saves
		saves += x.Saves
	}
	for _, g := range order {
		a := groups[g]
		fmt.Fprintf(&b, "%s: %d item(s), %s, frees %s\n", g, a.n, human(a.size), human(a.save))
	}
	fmt.Fprintf(&b, "total freed: %s\n", human(saves))
	for _, p := range problems {
		fmt.Fprintf(&b, "PROBLEM: %s\n", p)
	}
	if all {
		b.WriteString("\n")
		for _, x := range rs {
			if x.Action != "rows" {
				fmt.Fprintf(&b, "%-6s %9s  %s  [%s]\n", x.Action, human(x.Size), x.Path, x.Kind)
			}
		}
	}
	return b.String()
}

// group folds "job 141 (ended Nov 26)" into "job files", "scratch (job 3…)" into "scratch".
func group(kind string) string {
	switch {
	case strings.HasPrefix(kind, "job "):
		if strings.Contains(kind, "over ") {
			return "job files (size cap)"
		}
		return "job files"
	case strings.HasPrefix(kind, "scratch"):
		return "job scratch dirs"
	}
	return kind
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
