// Package config reads exo's settings from the environment, filled from an
// env file (EXO_ENV_FILE, default /etc/exo/exo.env), and the people it
// answers from a JSON file (EXO_PEOPLE, default <data>/people.json).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Person is someone the assistant answers. Messages from anyone else are dropped.
type Person struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Admin    bool   `json:"admin"`   // server tools, notes, jobs
	Discord  string `json:"discord"` // Discord user ID
	HomeChat string `json:"home"`    // where unsolicited messages go, e.g. "discord"
}

type Config struct {
	Name    string // what the assistant is called in prompts and notices
	DataDir string
	Loc     *time.Location

	DashAddr string // dashboard; keep it on localhost or a private network
	MCPAddr  string // only Claude Code talks to it
	MCPToken string

	DryRun          bool // log outbound messages instead of sending
	ScheduleEnabled bool
	Failover        string // ask | auto | off

	DiscordToken   string
	DiscordEnabled bool

	ClaudeBin    string
	ClaudeEnv    string // env file with the Claude login (CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY) for background jobs
	Model        string
	Effort       string
	DeepModel    string // a sub-agent chats and jobs can hand quality-over-speed work to ("" = none)
	ProbeModel   string
	TurnTimeout  time.Duration // Claude turns
	LocalTimeout time.Duration // local turns: thinking is slow on modest hardware, and a late answer beats a broken one
	JobTimeout   time.Duration
	CheckIn      time.Duration // a chat turn still running this long sends a progress note (0 = never)
	MaxJobs      int

	LocalURL       string // OpenAI-compatible endpoint (llama.cpp's llama-server, vLLM, Ollama)
	LocalModel     string
	LocalStart     string // optional command run before failing over to the local model (start the server)
	LocalStop      string // optional command run after switching back (stop it again)
	LocalVariant   string // how hard the local model thinks: fast | think | think<N> | effort-<level> (engine/profile.go)
	LocalMaxTokens int    // answer allowance per model call, on top of any thinking budget
	Shadow         string // shadow variants run on the local model after each Claude chat turn, e.g. "fast,think"; "" = off

	Transcribe string // command that prints a transcript of the audio file given as its last argument ("" = off)

	NotesMCP           string // MCP server for long-term notes (Basic Memory or anything with search_notes/read_note)
	NotesSearch        string // search_type for note hints: vector (default) | hybrid | text
	Heartbeat          string // cron for the heartbeat; "" = off (Run now still works)
	QuietFrom, QuietTo int    // the heartbeat never messages in [from, to) local hours

	People []*Person
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := strings.ToLower(env(k, ""))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(env(k, "")); err == nil {
		return n
	}
	return def
}

// loadEnvFile fills unset variables from the env file, so CLI subcommands
// run by hand see the same settings as the unit (systemd's EnvironmentFile
// syntax: KEY=VALUE per line, # comments).
func loadEnvFile(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func Load() (*Config, error) {
	if os.Getenv("EXO_MCP_TOKEN") == "" {
		loadEnvFile(env("EXO_ENV_FILE", "/etc/exo/exo.env"))
	}
	loc, err := time.LoadLocation(env("EXO_TZ", "Local"))
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	c := &Config{
		Name:            env("EXO_NAME", "Exo"),
		DataDir:         env("EXO_DATA", "/var/lib/exo"),
		Loc:             loc,
		DashAddr:        env("EXO_DASH_ADDR", "127.0.0.1:8656"),
		MCPAddr:         env("EXO_MCP_ADDR", "127.0.0.1:8655"),
		MCPToken:        env("EXO_MCP_TOKEN", ""),
		DryRun:          envBool("EXO_DRY_RUN", true),
		ScheduleEnabled: envBool("EXO_SCHEDULE", false),
		Failover:        env("EXO_FAILOVER", "ask"),
		DiscordToken:    env("EXO_DISCORD_TOKEN", ""),
		DiscordEnabled:  envBool("EXO_DISCORD", false),
		ClaudeBin:       env("EXO_CLAUDE_BIN", filepath.Join(home, ".local/bin/claude")),
		ClaudeEnv:       env("EXO_CLAUDE_ENV", "/etc/exo/claude.env"),
		Model:           env("EXO_MODEL", "claude-sonnet-5-5"),
		Effort:          env("EXO_EFFORT", "medium"),
		DeepModel:       env("EXO_DEEP_MODEL", "claude-opus-5-5"),
		ProbeModel:      env("EXO_PROBE_MODEL", "claude-haiku-5-5"),
		TurnTimeout:     time.Duration(envInt("EXO_TURN_TIMEOUT_S", 420)) * time.Second,
		LocalTimeout:    time.Duration(envInt("EXO_LOCAL_TIMEOUT_S", 600)) * time.Second,
		JobTimeout:      time.Duration(envInt("EXO_JOB_TIMEOUT_M", 90)) * time.Minute,
		CheckIn:         time.Duration(envInt("EXO_CHECKIN_S", 180)) * time.Second,
		MaxJobs:         envInt("EXO_MAX_JOBS", 2),
		LocalURL:        strings.TrimRight(env("EXO_LOCAL_URL", "http://127.0.0.1:8080/v1"), "/"),
		LocalModel:      env("EXO_LOCAL_MODEL", "local"),
		LocalStart:      env("EXO_LOCAL_START", ""),
		LocalStop:       env("EXO_LOCAL_STOP", ""),
		LocalVariant:    env("EXO_LOCAL_VARIANT", "fast"),
		LocalMaxTokens:  envInt("EXO_LOCAL_MAX_TOKENS", 2048),
		Shadow:          env("EXO_SHADOW", ""),
		Transcribe:      env("EXO_TRANSCRIBE", ""),
		NotesMCP:        env("EXO_NOTES_MCP", ""),
		NotesSearch:     env("EXO_NOTES_SEARCH", "vector"),
		Heartbeat:       env("EXO_HEARTBEAT", ""),
		QuietFrom:       envInt("EXO_QUIET_FROM", 22),
		QuietTo:         envInt("EXO_QUIET_TO", 7),
	}
	if c.MCPToken == "" {
		return nil, fmt.Errorf("EXO_MCP_TOKEN is required")
	}
	people, err := LoadPeople(env("EXO_PEOPLE", c.Path("people.json")))
	if err != nil {
		return nil, err
	}
	c.People = people
	return c, nil
}

// LoadPeople reads the people file: a JSON list of Person.
func LoadPeople(path string) ([]*Person, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("people file: %w", err)
	}
	var ps []*Person
	if err := json.Unmarshal(raw, &ps); err != nil {
		return nil, fmt.Errorf("people file %s: %w", path, err)
	}
	for _, p := range ps {
		if p.ID == "" || p.Name == "" {
			return nil, fmt.Errorf("people file %s: every person needs an id and a name", path)
		}
	}
	return ps, nil
}

func (c *Config) Person(id string) *Person {
	for _, p := range c.People {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (c *Config) ByDiscord(id string) *Person {
	for _, p := range c.People {
		if p.Discord != "" && p.Discord == id {
			return p
		}
	}
	return nil
}

func (c *Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.DataDir}, parts...)...)
}

// Agents is the --agents JSON for Claude Code: one "deep" sub-agent on
// DeepModel that a chat or job hands quality-over-speed work to. It inherits
// the caller's tools (exo's MCP included). "" when DeepModel is unset.
func (c *Config) Agents() string {
	if c.DeepModel == "" {
		return ""
	}
	j, _ := json.Marshal(map[string]any{"deep": map[string]any{
		"description": "Slower and more expensive, but deeper. Hand it work where quality beats speed: " +
			"careful research or comparisons, tricky reasoning, planning, debugging, or writing that has to be right. " +
			"Give it the full goal and everything it needs; it can't see the conversation.",
		"prompt": "You're the deep-work helper for " + c.Name + ", a personal assistant. Do the task you're given thoroughly, " +
			"checking facts with your tools rather than memory, and return a complete, accurate result to pass on. " +
			"Say plainly what you couldn't verify.",
		"model": c.DeepModel,
	}})
	return string(j)
}
