package main

import (
	"encoding/json"
	"fmt"

	"github.com/nathaniasSC2/exoskeleton/agent"
	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/store"
	"github.com/nathaniasSC2/exoskeleton/tools"
)

// budget prints what exo itself puts into every model call: the system
// prompt and the tool schemas the model actually sees (chars, ~tokens at 4
// chars each), next to what the toolbox can reach. Claude Code adds its own
// fixed overhead on top: ~390 tokens, plus ~1,020 for WebSearch/WebFetch.
func budget(cfg *config.Config, st *store.Store) error {
	a := &agent.Agent{Cfg: cfg, Store: st}
	reg := tools.New(cfg, st, nil, nil)
	a.Reg = reg
	if err := a.Init(); err != nil {
		return err
	}
	size := func(ts []*tools.Tool) int {
		var list []map[string]any
		for _, t := range ts {
			list = append(list, map[string]any{"name": t.Name, "description": t.Desc, "inputSchema": t.Schema()})
		}
		j, _ := json.Marshal(list)
		return len(j)
	}
	for _, p := range cfg.People {
		sys := a.SystemPromptFor(p)
		fmt.Printf("%s system prompt: %d chars (~%d tok)\n", p.ID, len(sys), len(sys)/4)
		for _, eng := range []string{"claude", "local"} {
			c := &tools.Ctx{Person: p, Engine: eng}
			seen, all := reg.Exposed(c), reg.Toolset(c)
			fmt.Printf("  %s on %s: model sees %d tool(s) = %d chars (~%d tok); the toolbox reaches %d tools (%d chars if all were loaded)\n",
				p.ID, eng, len(seen), size(seen), size(seen)/4, len(all), size(all))
		}
	}
	return nil
}
