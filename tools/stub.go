package tools

import "fmt"

// Replays and shadow runs never perform side effects, but a bare "skipped"
// teaches the model the wrong thing: in shadow runs a local model retried
// start_job seven times, or decided a kit was offline. replayStub answers the
// way the real tool would on success, so the rest of the turn is what a live
// turn would have been. Read-only actions of mixed tools (reminder list) really
// run; see replayRuns.

func replayRuns(t *Tool, a Args) bool {
	if t.ReplayRuns != nil {
		return t.ReplayRuns(a)
	}
	switch t.Name {
	case "shell":
		return ReadOnlyCommand(a.S("command"))
	case "reminder", "routine":
		return a.S("action") == "list"
	}
	return false
}

func (c *Ctx) replayStub(name string, a Args) string {
	c.mu.Lock()
	c.stubs++
	n := 900 + c.stubs
	c.mu.Unlock()
	who := "the person"
	if c.Person != nil {
		who = c.Person.Name
	}
	switch name {
	case "start_job":
		dir := a.S("dir")
		if dir == "" {
			dir = "~"
		}
		return fmt.Sprintf("job #%d started in %s; its report goes to %s when it finishes (don't promise progress updates)", n, dir, who)
	case "cancel_job":
		return fmt.Sprintf("job #%s cancelled", a.S("id"))
	case "remember":
		return fmt.Sprintf("saved as fact #%d", n)
	case "forget":
		return "forgotten"
	case "reminder":
		return fmt.Sprintf("reminder #%d set", n)
	case "routine":
		return fmt.Sprintf("routine #%d saved", n)
	case "notes_append":
		return "appended to " + a.S("note")
	case "send_file":
		return "it will be sent with your reply"
	}
	if a.S("task") != "" { // a kit
		return fmt.Sprintf("started job #%d with the %s kit; the result (and any files) goes to %s when it's done", n, name, who)
	}
	return "done"
}
