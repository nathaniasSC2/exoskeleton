package jobs

import (
	"strings"
	"testing"

	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/tools"
)

// A job's own choices are checked before anything is stored or started.
func TestStartOptsRejects(t *testing.T) {
	r := &Runner{Cfg: &config.Config{DataDir: t.TempDir()}, kits: map[string]tools.Kit{}, builtin: map[string]tools.Kit{}}
	for _, c := range []struct {
		o    Opts
		task string
		want string
	}{
		{Opts{Kit: "nope"}, "do it", "unknown kit"},
		{Opts{Effort: "extreme"}, "do it", "unknown effort"},
		{Opts{Model: "--dangerously-skip-permissions"}, "do it", "bad model name"},
		{Opts{Model: "a model"}, "do it", "bad model name"},
		{Opts{}, "   ", "empty task"},
	} {
		_, err := r.StartOpts("alex", "console", "c", c.task, "/tmp", c.o)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("StartOpts(%+v, %q) = %v, want an error containing %q", c.o, c.task, err, c.want)
		}
	}
}
