package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/nathaniasSC2/exoskeleton/sched"
)

// Command runs the maintenance subcommands a deployment's CLI can pass on:
//
//	prune [-n] [-all]          apply the retention now (-n: only say what would go)
//	temp list                  the temp-change ledger
//	temp add -id ID -what W -expires DATE [-undo CMD] [-auto] [-by WHO]
//	temp check [-n]            revert or ask about what has expired
//	temp undo ID               run a change's undo now
//	temp done ID [-note N]     mark a change put back by hand
//
// It reports whether name was one of these.
func (ap *App) Command(ctx context.Context, name string, args []string, w io.Writer) (bool, error) {
	switch name {
	case "prune":
		fs := flag.NewFlagSet("prune", flag.ContinueOnError)
		dry := fs.Bool("n", false, "dry run: list what would be removed, change nothing")
		all := fs.Bool("all", false, "list every path, not just the totals")
		if err := fs.Parse(args); err != nil {
			return true, err
		}
		rs, problems := ap.Prune(ctx, *dry)
		if *dry {
			fmt.Fprintln(w, "DRY RUN: nothing was changed.")
		}
		fmt.Fprint(w, FormatPrune(rs, problems, *all))
		return true, nil
	case "temp":
		return true, ap.tempCmd(ctx, args, w)
	}
	return false, nil
}

func (ap *App) tempCmd(ctx context.Context, args []string, w io.Writer) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list":
		cs, err := ap.TempChanges()
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			fmt.Fprintln(w, "no temporary changes recorded")
		}
		for _, c := range cs {
			mode := "asks first"
			if c.Auto {
				mode = "auto undo"
			}
			fmt.Fprintf(w, "%-9s %s  expires %s  (%s, by %s)\n          %s\n", c.Status, c.ID, c.Expires, mode, orNone(c.By), c.What)
			if c.Undo != "" {
				fmt.Fprintf(w, "          undo: %s\n", c.Undo)
			}
			if c.Result != "" {
				fmt.Fprintf(w, "          result: %s\n", strings.ReplaceAll(c.Result, "\n", " / "))
			}
		}
		return nil
	case "add":
		fs := flag.NewFlagSet("temp add", flag.ContinueOnError)
		var c TempChange
		fs.StringVar(&c.ID, "id", "", "short name (lowercase, digits, . _ -)")
		fs.StringVar(&c.What, "what", "", "what was changed, in a line")
		fs.StringVar(&c.Expires, "expires", "", "YYYY-MM-DD (end of that day) or an RFC 3339 time")
		fs.StringVar(&c.Undo, "undo", "", "bash command that puts it back")
		fs.BoolVar(&c.Auto, "auto", false, "run the undo when it expires (otherwise the admin is asked)")
		fs.StringVar(&c.By, "by", "", "who made the change")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if err := ap.AddTempChange(c); err != nil {
			return err
		}
		exp, _ := sched.ParseUntil(c.Expires, ap.Cfg.Loc)
		fmt.Fprintf(w, "recorded %q: expires %s\n", c.ID, exp.In(ap.Cfg.Loc).Format("Mon Jan 2 3:04 PM"))
		return nil
	case "check":
		fs := flag.NewFlagSet("temp check", flag.ContinueOnError)
		dry := fs.Bool("n", false, "only say what would happen")
		if err := fs.Parse(args); err != nil {
			return err
		}
		msgs := ap.TempCheck(ctx, *dry)
		if len(msgs) == 0 {
			fmt.Fprintln(w, "nothing has expired")
		}
		for _, m := range msgs {
			fmt.Fprintln(w, m)
		}
		return nil
	case "undo":
		if len(args) != 1 {
			return fmt.Errorf("usage: temp undo ID")
		}
		out, err := ap.UndoTempChange(ctx, args[0])
		if out != "" {
			fmt.Fprintln(w, out)
		}
		if err == nil {
			fmt.Fprintf(w, "%s reverted\n", args[0])
		}
		return err
	case "done":
		fs := flag.NewFlagSet("temp done", flag.ContinueOnError)
		note := fs.String("note", "", "what was done")
		if len(args) < 1 {
			return fmt.Errorf("usage: temp done ID [-note TEXT]")
		}
		id := args[0]
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return ap.DoneTempChange(id, *note)
	}
	return fmt.Errorf("unknown temp command %q (list, add, check, undo, done)", sub)
}
