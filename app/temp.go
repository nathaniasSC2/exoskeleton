package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/sched"
)

// The temp-change ledger: every temporary change to the box (an env value
// raised for a busy day, an extra schedule entry, a unit started for an
// event) is written down with when it expires and how to undo it, and
// nothing has to remember to put it back. A holiday's raised MAX_JOBS and its
// extra schedule entry were noticed by two heartbeats and undone by hand the
// next morning; this is that, as code.
//
// The ledger is <data>/temp-changes.json. Schedule entries carry their own
// "until" instead (sched.Entry.Until).

// TempChange is one temporary change.
type TempChange struct {
	ID      string `json:"id"`               // short name, unique in the ledger
	What    string `json:"what"`             // what was changed, in a line
	By      string `json:"by,omitempty"`     // who made it
	Made    string `json:"made"`             // RFC 3339
	Expires string `json:"expires"`          // RFC 3339, or YYYY-MM-DD (the end of that local day)
	Undo    string `json:"undo,omitempty"`   // a bash command that puts it back
	Auto    bool   `json:"auto,omitempty"`   // run Undo when it expires; otherwise ask the admin first
	Status  string `json:"status"`           // active | reverting | reverted | failed | asked
	Result  string `json:"result,omitempty"` // what the undo printed, or why it's waiting
	Done    string `json:"done,omitempty"`   // when it was reverted or asked about (RFC 3339)
}

var tempID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,40}$`)

func (ap *App) tempPath() string { return ap.Cfg.Path("temp-changes.json") }

// TempChanges reads the ledger (a missing file is an empty ledger).
func (ap *App) TempChanges() ([]TempChange, error) {
	b, err := os.ReadFile(ap.tempPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []TempChange
	if strings.TrimSpace(string(b)) == "" {
		return nil, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", ap.tempPath(), err)
	}
	return out, nil
}

func (ap *App) saveTemp(cs []TempChange) error {
	b, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	tmp := ap.tempPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o664); err != nil {
		return err
	}
	return os.Rename(tmp, ap.tempPath())
}

// AddTempChange records a temporary change. Expires must parse and be in the
// future; ids are unique among changes that aren't finished.
func (ap *App) AddTempChange(c TempChange) error {
	c.ID = strings.TrimSpace(c.ID)
	if !tempID.MatchString(c.ID) {
		return fmt.Errorf("id %q: use lowercase letters, digits, . _ -", c.ID)
	}
	if strings.TrimSpace(c.What) == "" {
		return fmt.Errorf("what is required")
	}
	exp, err := sched.ParseUntil(c.Expires, ap.Cfg.Loc)
	if err != nil || exp.IsZero() {
		return fmt.Errorf("expires %q: use YYYY-MM-DD or an RFC 3339 time", c.Expires)
	}
	if !exp.After(time.Now()) {
		return fmt.Errorf("expires %q is already past", c.Expires)
	}
	if c.Auto && strings.TrimSpace(c.Undo) == "" {
		return fmt.Errorf("auto needs an undo command")
	}
	cs, err := ap.TempChanges()
	if err != nil {
		return err
	}
	for _, x := range cs {
		if x.ID == c.ID && (x.Status == "active" || x.Status == "asked" || x.Status == "reverting") {
			return fmt.Errorf("%q is already in the ledger (%s)", c.ID, x.Status)
		}
	}
	if c.Made == "" {
		c.Made = time.Now().Format(time.RFC3339)
	}
	c.Status, c.Result, c.Done = "active", "", ""
	return ap.saveTemp(append(cs, c))
}

// TempCheck handles what's expired: changes with auto undo are reverted,
// the rest asked about once (and reported again every night while they're
// still in place), and schedule entries past their until are taken out of
// schedule.json. It returns the messages for the admin. With dry it only
// says what it would do.
func (ap *App) TempCheck(ctx context.Context, dry bool) []string {
	var msgs []string
	now := time.Now()
	cs, err := ap.TempChanges()
	if err != nil {
		return []string{"Temp-change ledger unreadable: " + err.Error()}
	}
	changed := false
	for i := range cs {
		c := &cs[i]
		exp, err := sched.ParseUntil(c.Expires, ap.Cfg.Loc)
		switch {
		case c.Status == "reverting":
			// The undo was started and nothing recorded how it ended: it most
			// likely restarted this process. Don't run it twice.
			if done, _ := time.Parse(time.RFC3339, c.Done); now.Sub(done) > 15*time.Minute && !dry {
				c.Status, c.Result = "reverted", "the undo started at "+c.Done+"; its end wasn't recorded (it probably restarted me)"
				changed = true
			}
			continue
		case c.Status != "active" && c.Status != "asked":
			continue
		case err != nil || exp.IsZero():
			msgs = append(msgs, fmt.Sprintf("Temp change %q has an unreadable expiry %q; fix it in %s.", c.ID, c.Expires, ap.tempPath()))
			continue
		case now.Before(exp):
			continue
		}
		if c.Status == "asked" {
			if sched.Manual(ctx) || isMaintenance(ctx) {
				msgs = append(msgs, fmt.Sprintf("Temp change %q is still in place, %s past its expiry: %s. Undo: %s", c.ID, human2(now.Sub(exp)), c.What, orNone(c.Undo)))
			}
			continue
		}
		if dry {
			if c.Auto {
				msgs = append(msgs, fmt.Sprintf("would revert %q (%s): %s", c.ID, c.What, c.Undo))
			} else {
				msgs = append(msgs, fmt.Sprintf("would ask about %q (%s)", c.ID, c.What))
			}
			continue
		}
		if !c.Auto {
			c.Status, c.Done = "asked", now.Format(time.RFC3339)
			c.Result = "expired; waiting for a go-ahead"
			changed = true
			undo := "no undo command was recorded, so it needs doing by hand"
			if c.Undo != "" {
				undo = fmt.Sprintf("say so and I'll run `%s temp undo %s` (%s)", ap.Cfg.Slug(), c.ID, c.Undo)
			}
			msgs = append(msgs, fmt.Sprintf("A temporary change has expired and is still in place: %s (made %s by %s). To put it back, %s.", c.What, day(c.Made), orNone(c.By), undo))
			continue
		}
		c.Status, c.Done = "reverting", now.Format(time.RFC3339)
		if err := ap.saveTemp(cs); err != nil {
			return append(msgs, "Temp-change ledger not saved, nothing reverted: "+err.Error())
		}
		out, err := ap.runUndo(ctx, c.ID, c.Undo)
		c.Result, c.Done = out, time.Now().Format(time.RFC3339)
		if err != nil {
			c.Status = "failed"
			msgs = append(msgs, fmt.Sprintf("Couldn't revert the temporary change %q (%s): %v. %s", c.ID, c.What, err, out))
		} else {
			c.Status = "reverted"
			msgs = append(msgs, fmt.Sprintf("Put back a temporary change that expired: %s.", c.What))
		}
		changed = true
	}
	if changed {
		if err := ap.saveTemp(cs); err != nil {
			msgs = append(msgs, "Temp-change ledger not saved: "+err.Error())
		}
	}
	return append(msgs, ap.expireSchedule(dry)...)
}

// UndoTempChange runs one change's undo now (the admin said to).
func (ap *App) UndoTempChange(ctx context.Context, id string) (string, error) {
	cs, err := ap.TempChanges()
	if err != nil {
		return "", err
	}
	for i := range cs {
		c := &cs[i]
		if c.ID != id || (c.Status != "active" && c.Status != "asked" && c.Status != "failed") {
			continue
		}
		if strings.TrimSpace(c.Undo) == "" {
			return "", fmt.Errorf("%q has no undo command; put it back by hand, then `%s temp done %s`", id, ap.Cfg.Slug(), id)
		}
		c.Status, c.Done = "reverting", time.Now().Format(time.RFC3339)
		if err := ap.saveTemp(cs); err != nil {
			return "", err
		}
		out, err := ap.runUndo(ctx, c.ID, c.Undo)
		c.Result, c.Done = out, time.Now().Format(time.RFC3339)
		c.Status = "reverted"
		if err != nil {
			c.Status = "failed"
		}
		if serr := ap.saveTemp(cs); serr != nil && err == nil {
			err = serr
		}
		return out, err
	}
	return "", fmt.Errorf("no open temp change %q", id)
}

// DoneTempChange marks a change as put back by hand.
func (ap *App) DoneTempChange(id, note string) error {
	cs, err := ap.TempChanges()
	if err != nil {
		return err
	}
	for i := range cs {
		if cs[i].ID == id && cs[i].Status != "reverted" {
			cs[i].Status, cs[i].Done = "reverted", time.Now().Format(time.RFC3339)
			cs[i].Result = strings.TrimSpace("marked done by hand. " + note)
			return ap.saveTemp(cs)
		}
	}
	return fmt.Errorf("no open temp change %q", id)
}

// runUndo runs an undo command as its own transient unit, so an undo that
// restarts this process (an env change needs one) isn't killed with it.
func (ap *App) runUndo(ctx context.Context, id, undo string) (string, error) {
	if ap.undo != nil {
		return ap.undo(ctx, id, undo)
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	unit := fmt.Sprintf("%s-undo-%s-%d", ap.Cfg.Slug(), strings.NewReplacer(".", "-", "_", "-").Replace(id), time.Now().Unix())
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sudo", "-n", "systemd-run", "--unit="+unit, "--collect", "--quiet", "--wait", "--pipe",
		"--uid="+u.Uid, "--gid="+u.Gid, "--working-directory="+ap.Cfg.DataDir, "-p", "RuntimeMaxSec=600",
		"--setenv=HOME="+u.HomeDir, "--setenv=PATH="+u.HomeDir+"/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--", "bash", "-c", undo)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s, err
}

// expireSchedule takes entries past their until out of schedule.json (the
// scheduler already skips them), keeping a .bak of the file first.
func (ap *App) expireSchedule(dry bool) []string {
	path := ap.Cfg.Path("schedule.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return []string{"schedule.json unreadable: " + err.Error()}
	}
	var keep []json.RawMessage
	var msgs, gone []string
	for _, e := range entries {
		var s sched.Entry
		json.Unmarshal(e, &s)
		if _, err := sched.ParseUntil(s.Until, ap.Cfg.Loc); err != nil {
			msgs = append(msgs, fmt.Sprintf("Schedule entry %q has an unreadable until %q, so it never expires; fix it in schedule.json.", s.Name, s.Until))
		}
		if s.Expired(time.Now(), ap.Cfg.Loc) {
			gone = append(gone, s.Name)
			continue
		}
		keep = append(keep, e)
	}
	if len(gone) == 0 {
		return msgs
	}
	sort.Strings(gone)
	if dry {
		return append(msgs, "would take expired entries out of schedule.json: "+strings.Join(gone, ", "))
	}
	if err := os.WriteFile(path+".bak-"+time.Now().Format("20060102-150405")+"-expired", raw, 0o644); err != nil {
		return append(msgs, "schedule.json backup failed, expired entries left in: "+err.Error())
	}
	b, _ := json.MarshalIndent(keep, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return append(msgs, "schedule.json not rewritten: "+err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		return append(msgs, "schedule.json not rewritten: "+err.Error())
	}
	return append(msgs, "Took temporary schedule entries out now that they've expired: "+strings.Join(gone, ", ")+".")
}

type maintKey struct{}

func isMaintenance(ctx context.Context) bool { v, _ := ctx.Value(maintKey{}).(bool); return v }

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func day(rfc string) string {
	if t, err := time.Parse(time.RFC3339, rfc); err == nil {
		return t.Format("Jan 2")
	}
	return rfc
}

func human2(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}
