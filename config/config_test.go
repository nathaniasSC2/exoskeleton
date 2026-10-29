package config

import "testing"

func TestLoadBrand(t *testing.T) {
	t.Setenv("ACME_MCP_TOKEN", "x")
	t.Setenv("ACME_MODEL", "m-from-env")
	t.Setenv("EXO_MODEL", "ignored")
	b := Brand{Prefix: "ACME", Slug: "acme",
		Defaults: map[string]string{"NAME": "Acme", "DATA": t.TempDir(), "QUIET_TO": "6", "DRY_RUN": "0", "MODEL": "m-default"},
		People: func(c *Config) ([]*Person, error) {
			return []*Person{{ID: "alex", Name: "Alex", Admin: true, Handles: map[string][]string{"sms": {"+15550100"}}}}, nil
		}}
	c, err := LoadBrand(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Acme" || c.QuietTo != 6 || c.DryRun || c.Model != "m-from-env" || c.Slug() != "acme" {
		t.Fatalf("got name %q quiet_to %d dry_run %v model %q slug %q", c.Name, c.QuietTo, c.DryRun, c.Model, c.Slug())
	}
	if c.ClaudeEnv != "/etc/acme/claude.env" || c.BackupDir != c.Path("backups") {
		t.Fatalf("claude env %q, backups %q", c.ClaudeEnv, c.BackupDir)
	}
	if p := c.ByHandle("sms", "+15550100"); p == nil || p.ID != "alex" {
		t.Fatal("ByHandle missed")
	}
	if c.ByHandle("discord", "+15550100") != nil || c.ByHandle("sms", "") != nil {
		t.Fatal("ByHandle matched the wrong channel or an empty id")
	}
	t.Setenv("ACME_EXTRA", "on")
	if c.Env("EXTRA", "") != "on" || c.Env("NAME", "") != "Acme" {
		t.Fatal("Config.Env should read the brand's variables and defaults")
	}
}

func TestLoadNeedsToken(t *testing.T) {
	t.Setenv("ACME_ENV_FILE", "/nonexistent")
	if _, err := LoadBrand(Brand{Prefix: "ACME", Slug: "acme"}); err == nil {
		t.Fatal("expected an error without ACME_MCP_TOKEN")
	}
}
