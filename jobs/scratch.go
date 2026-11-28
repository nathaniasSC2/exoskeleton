package jobs

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A job's scratch directories (Cfg.JobScratch, e.g. /tmp/j{id}) hold renders,
// frames and throwaway scripts. Nothing else cleans /tmp for weeks, and on
// 2026-11-27 22 of them held 6 GB, so they go when their job ends, and
// maintenance sweeps up the ones a crash or a restart left behind.

// ScratchDirs are job id's scratch directories.
func (r *Runner) ScratchDirs(id int64) []string {
	var out []string
	for _, p := range r.Cfg.JobScratch {
		out = append(out, strings.ReplaceAll(p, "{id}", strconv.FormatInt(id, 10)))
	}
	return out
}

// removeScratch deletes an ended job's scratch directories (only real
// directories this user owns; a symlink or someone else's is left alone).
func (r *Runner) removeScratch(id int64) {
	for _, d := range r.ScratchDirs(id) {
		if ownDir(d) {
			os.RemoveAll(d)
		}
	}
}

// Scratch is one leftover scratch directory.
type Scratch struct {
	Path   string
	Job    int64
	Bytes  int64
	Newest time.Time // the newest modification anywhere inside it
}

// StaleScratch lists scratch directories of ended jobs whose newest file is
// older than age. While any job is queued or running it lists nothing: a
// running job may be reading another one's leftovers.
func (r *Runner) StaleScratch(age time.Duration) []Scratch {
	var n int
	r.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status IN ('queued','running')`).Scan(&n)
	if n > 0 {
		return nil
	}
	var out []Scratch
	for _, pat := range r.Cfg.JobScratch {
		re := regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(pat), regexp.QuoteMeta("{id}"), `([0-9]+)`) + "$")
		matches, _ := filepath.Glob(strings.ReplaceAll(pat, "{id}", "[0-9]*"))
		for _, m := range matches {
			sm := re.FindStringSubmatch(m)
			if sm == nil || !ownDir(m) {
				continue
			}
			id, _ := strconv.ParseInt(sm[1], 10, 64)
			var status string
			if r.Store.DB.QueryRow(`SELECT status FROM jobs WHERE id=?`, id).Scan(&status) != nil {
				continue // not one of our jobs
			}
			s := Scratch{Path: m, Job: id}
			filepath.WalkDir(m, func(_ string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if info, err := d.Info(); err == nil {
					if info.ModTime().After(s.Newest) {
						s.Newest = info.ModTime()
					}
					if info.Mode().IsRegular() {
						s.Bytes += info.Size()
					}
				}
				return nil
			})
			if time.Since(s.Newest) > age {
				out = append(out, s)
			}
		}
	}
	return out
}

func ownDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	uid, ok := fileOwner(info)
	return ok && uid == os.Getuid()
}
