package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadKits(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kits.json")
	os.WriteFile(p, []byte(`{"render": {"short": "3D renders", "dir": "/srv/render", "brief": "Check every render.",
		"mcp": {"type": "http", "url": "http://127.0.0.1:9000/mcp"}}}`), 0o644)
	kits := loadKits(p)
	k, ok := kits["render"]
	if !ok || k.Name != "render" || k.Dir != "/srv/render" || k.MCP["url"] == nil {
		t.Fatalf("got %+v", kits)
	}
	if len(loadKits(filepath.Join(t.TempDir(), "missing.json"))) != 0 {
		t.Fatal("a missing kits.json should mean no kits")
	}
}
