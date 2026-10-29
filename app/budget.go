package app

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/nathaniasSC2/exoskeleton/tools"
)

// Budget prints what the harness itself puts into every model call: the
// system prompt and the tool schemas the model actually sees (chars, ~tokens
// at 4 chars each), next to what the toolbox can reach. Claude Code adds its
// own fixed overhead on top: ~390 tokens, plus ~1,020 for WebSearch/WebFetch.
func (ap *App) Budget(w io.Writer) {
	size := func(ts []*tools.Tool) int {
		var list []map[string]any
		for _, t := range ts {
			list = append(list, map[string]any{"name": t.Name, "description": t.Desc, "inputSchema": t.Schema()})
		}
		j, _ := json.Marshal(list)
		return len(j)
	}
	for _, p := range ap.Cfg.People {
		sys := ap.Agent.SystemPromptFor(p)
		fmt.Fprintf(w, "%s system prompt: %d chars (~%d tok)\n", p.ID, len(sys), len(sys)/4)
		for _, eng := range []string{"claude", "local"} {
			c := &tools.Ctx{Person: p, Engine: eng}
			seen, all := ap.Reg.Exposed(c), ap.Reg.Toolset(c)
			fmt.Fprintf(w, "  %s on %s: model sees %d tool(s) = %d chars (~%d tok); the toolbox reaches %d tools (%d chars if all were loaded)\n",
				p.ID, eng, len(seen), size(seen), size(seen)/4, len(all), size(all))
		}
	}
}
