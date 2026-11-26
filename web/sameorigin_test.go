package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/agent"
	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/store"
)

const host = "dash.example.ts.net:8448"

// tailnet makes r look like it came through tailscale serve.
func tailnet(r *http.Request) *http.Request {
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("X-Forwarded-For", "100.64.0.9")
	r.Header.Set("X-Forwarded-Host", host)
	r.Header.Set("Tailscale-User-Login", "owner@example.com")
	r.Header.Set("Tailscale-Headers-Info", "https://tailscale.com/s/serve-headers")
	return r
}

func TestCrossSite(t *testing.T) {
	cases := []struct {
		name   string
		method string
		hdr    map[string]string
		via    string // "tailnet", "local" or "lan"
		ok     bool
	}{
		{"GET is never blocked", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, "tailnet", true},
		{"same-origin form", "POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://" + host}, "tailnet", true},
		{"typed or bookmarked", "POST", map[string]string{"Sec-Fetch-Site": "none"}, "tailnet", true},
		{"forged cross-site", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, "tailnet", false},
		{"forged cross-site even with a matching Origin", "POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://" + host}, "tailnet", false},
		{"another app on the same host", "POST", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://dash.example.ts.net:8447"}, "tailnet", false},
		{"old browser, own Origin", "POST", map[string]string{"Origin": "https://" + host}, "tailnet", true},
		{"old browser, evil Origin", "POST", map[string]string{"Origin": "https://evil.example"}, "tailnet", false},
		{"Origin null", "POST", map[string]string{"Origin": "null"}, "tailnet", false},
		{"evil Origin straight to loopback", "POST", map[string]string{"Origin": "https://evil.example"}, "local", false},
		{"no headers through tailscale serve", "POST", nil, "tailnet", false},
		{"no headers from a local script", "POST", nil, "local", true},
		{"no headers from the LAN", "POST", nil, "lan", false},
		{"DELETE forged", "DELETE", map[string]string{"Sec-Fetch-Site": "cross-site"}, "tailnet", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://"+host+"/mode", nil)
		switch c.via {
		case "tailnet":
			tailnet(r)
		case "local":
			r.Host = "127.0.0.1:8656"
			r.RemoteAddr = "127.0.0.1:41000"
		case "lan":
			r.RemoteAddr = "192.168.0.50:41000"
		}
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		why := CrossSite(r)
		if (why == "") != c.ok {
			t.Errorf("%s: got %q, want ok=%v", c.name, why, c.ok)
		}
	}
}

// Forged requests against the real dashboard routes, the exo ones and a
// deployment's mounted ones, never reach their handlers.
func TestForgedRequestsRefused(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	a := &agent.Agent{Cfg: &config.Config{Name: "Exo", Loc: time.UTC}, Store: st}
	hits := 0
	s := &Server{Agent: a, Store: st, Mount: func(mux *http.ServeMux) {
		mux.HandleFunc("POST /server/t/{key}/power", func(w http.ResponseWriter, r *http.Request) { hits++ })
	}}
	h := s.Handler()
	do := func(path string, hdr map[string]string) int {
		r := tailnet(httptest.NewRequest("POST", "http://"+host+path, strings.NewReader("text=hi&mode=local")))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	forged := []map[string]string{
		{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
		{"Origin": "https://evil.example"},
		{"Sec-Fetch-Site": "cross-site"},
	}
	for _, path := range []string{"/console", "/mode", "/schedule/backup/run", "/job/1/cancel", "/memory", "/prompts", "/replay", "/turn/1/feedback", "/shadow/eval", "/server/t/plex/power"} {
		for _, f := range forged {
			if code := do(path, f); code != http.StatusForbidden {
				t.Errorf("%s with %v: %d, want 403", path, f, code)
			}
		}
	}
	if hits != 0 {
		t.Fatalf("a forged request reached a handler %d times", hits)
	}
	if code := do("/server/t/plex/power", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://" + host}); code != 200 || hits != 1 {
		t.Fatalf("own page's form: %d, hits %d", code, hits)
	}

	// Over a real socket: a local script with no browser headers still works.
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := http.Post(srv.URL+"/server/t/plex/power", "application/x-www-form-urlencoded", strings.NewReader("on=1"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || hits != 2 {
		t.Fatalf("local script: %d, hits %d", res.StatusCode, hits)
	}
}
