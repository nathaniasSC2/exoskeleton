// Package config reads exo's settings from the environment, filled from an
// env file (EXO_ENV_FILE, default /etc/exo/exo.env), and the people it
// answers from a JSON file (EXO_PEOPLE, default <data>/people.json).
//
// A deployment built on exo can run under its own Brand: its own env prefix,
// defaults, and the short name used for the database, the MCP server, job
// units and backups.
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
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Admin    bool                `json:"admin"`             // server tools, notes, jobs
	Discord  string              `json:"discord"`           // Discord user ID
	HomeChat string              `json:"home"`              // channel unsolicited messages go to, e.g. "discord"
	Handles  map[string][]string `json:"handles,omitempty"` // ids on other channels: channel -> ids
	Extra    map[string]string   `json:"extra,omitempty"`   // settings for add-on channels and tools
}

// Brand is a deployment's identity. Prefix names its env variables
// (<Prefix>_DATA, ...); Slug names <slug>.db, the MCP server (tools show up
// as mcp__<slug>__*), job units (<slug>-job-<id>) and backups.
type Brand struct {
	Prefix, Slug string
	// Defaults replace exo's built-in defaults, keyed without the prefix
	// ("DATA", "TZ", ...). The environment still wins.
	Defaults map[string]string
	// People, if set, builds the people list instead of reading people.json.
	People func(c *Config) ([]*Person, error)
}

// Exo is the default brand.
var Exo = Brand{Prefix: "EXO", Slug: "exo"}

type Config struct {
	Brand   Brand
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
	BackupDir  string // nightly database copies, 14 kept (default <data>/backups)

	NotesMCP           string // MCP server for long-term notes (Basic Memory or anything with search_notes/read_note)
	NotesSearch        string // search_type for note hints: vector (default) | hybrid | text
	Heartbeat          string // cron for the heartbeat; "" = off (Run now still works)
	QuietFrom, QuietTo int    // the heartbeat never messages in [from, to) local hours

	People []*Person
}

// Env reads a variable, or def when it's unset or blank.
func Env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// EnvBool reads 1/true/yes/on or 0/false/no/off, else def.
func EnvBool(k string, def bool) bool {
	return parseBool(Env(k, ""), def)
}

func parseBool(v string, def bool) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// EnvInt reads an integer, else def.
func EnvInt(k string, def int) int {
	if n, err := strconv.Atoi(Env(k, "")); err == nil {
		return n
	}
	return def
}

// LoadEnvFile fills unset variables from the env file, so CLI subcommands
// run by hand see the same settings as the unit (systemd's EnvironmentFile
// syntax: KEY=VALUE per line, # comments).
func LoadEnvFile(path string) {
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

// Load reads the settings under the default brand.
func Load() (*Config, error) { return LoadBrand(Exo) }

// LoadBrand reads the settings for a brand: <Prefix>_* variables, filled from
// <Prefix>_ENV_FILE (default /etc/<slug>/<slug>.env).
func LoadBrand(b Brand) (*Config, error) {
	k := func(name string) string { return b.Prefix + "_" + name }
	if os.Getenv(k("MCP_TOKEN")) == "" {
		LoadEnvFile(Env(k("ENV_FILE"), "/etc/"+b.Slug+"/"+b.Slug+".env"))
	}
	env := func(name, def string) string {
		if d, ok := b.Defaults[name]; ok {
			def = d
		}
		return Env(k(name), def)
	}
	envBool := func(name string, def bool) bool {
		return parseBool(env(name, ""), def)
	}
	envInt := func(name string, def int) int {
		if n, err := strconv.Atoi(env(name, "")); err == nil {
			return n
		}
		return def
	}
	loc, err := time.LoadLocation(env("TZ", "Local"))
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	c := &Config{
		Brand:           b,
		Name:            env("NAME", "Exo"),
		DataDir:         env("DATA", "/var/lib/"+b.Slug),
		Loc:             loc,
		DashAddr:        env("DASH_ADDR", "127.0.0.1:8656"),
		MCPAddr:         env("MCP_ADDR", "127.0.0.1:8655"),
		MCPToken:        env("MCP_TOKEN", ""),
		DryRun:          envBool("DRY_RUN", true),
		ScheduleEnabled: envBool("SCHEDULE", false),
		Failover:        env("FAILOVER", "ask"),
		DiscordToken:    env("DISCORD_TOKEN", ""),
		DiscordEnabled:  envBool("DISCORD", false),
		ClaudeBin:       env("CLAUDE_BIN", filepath.Join(home, ".local/bin/claude")),
		ClaudeEnv:       env("CLAUDE_ENV", "/etc/"+b.Slug+"/claude.env"),
		Model:           env("MODEL", "claude-sonnet-5-5"),
		Effort:          env("EFFORT", "medium"),
		DeepModel:       env("DEEP_MODEL", "claude-opus-5-5"),
		ProbeModel:      env("PROBE_MODEL", "claude-haiku-5-5"),
		TurnTimeout:     time.Duration(envInt("TURN_TIMEOUT_S", 420)) * time.Second,
		LocalTimeout:    time.Duration(envInt("LOCAL_TIMEOUT_S", 600)) * time.Second,
		JobTimeout:      time.Duration(envInt("JOB_TIMEOUT_M", 90)) * time.Minute,
		CheckIn:         time.Duration(envInt("CHECKIN_S", 180)) * time.Second,
		MaxJobs:         envInt("MAX_JOBS", 2),
		LocalURL:        strings.TrimRight(env("LOCAL_URL", "http://127.0.0.1:8080/v1"), "/"),
		LocalModel:      env("LOCAL_MODEL", "local"),
		LocalStart:      env("LOCAL_START", ""),
		LocalStop:       env("LOCAL_STOP", ""),
		LocalVariant:    env("LOCAL_VARIANT", "fast"),
		LocalMaxTokens:  envInt("LOCAL_MAX_TOKENS", 2048),
		Shadow:          env("SHADOW", ""),
		Transcribe:      env("TRANSCRIBE", ""),
		NotesMCP:        env("NOTES_MCP", ""),
		NotesSearch:     env("NOTES_SEARCH", "vector"),
		Heartbeat:       env("HEARTBEAT", ""),
		QuietFrom:       envInt("QUIET_FROM", 22),
		QuietTo:         envInt("QUIET_TO", 7),
	}
	c.BackupDir = env("BACKUP_DIR", c.Path("backups"))
	if c.MCPToken == "" {
		return nil, fmt.Errorf("%s is required", k("MCP_TOKEN"))
	}
	var people []*Person
	if b.People != nil {
		people, err = b.People(c)
	} else {
		people, err = LoadPeople(env("PEOPLE", c.Path("people.json")))
	}
	if err != nil {
		return nil, err
	}
	c.People = people
	return c, nil
}

// Env reads one of the brand's own variables (<Prefix>_<name>), for settings
// an add-on defines; the brand's Defaults apply.
func (c *Config) Env(name, def string) string {
	if d, ok := c.Brand.Defaults[name]; ok {
		def = d
	}
	return Env(c.Brand.Prefix+"_"+name, def)
}

// Slug is the brand's short name ("exo" by default).
func (c *Config) Slug() string {
	if c.Brand.Slug == "" {
		return "exo"
	}
	return c.Brand.Slug
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

// ByHandle finds the person with this id on a channel (Person.Handles).
func (c *Config) ByHandle(channel, id string) *Person {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	for _, p := range c.People {
		for _, h := range p.Handles[channel] {
			if strings.EqualFold(h, id) {
				return p
			}
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
