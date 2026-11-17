package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nathaniasSC2/exoskeleton/agent"
	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/store"
)

// A deployment's own page renders inside exo's layout, with its nav link.
func TestExtraPages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	a := &agent.Agent{Cfg: &config.Config{Name: "Exo", Loc: time.UTC}, Store: st}
	s := &Server{Agent: a, Store: st,
		Nav:   []Link{{Href: "/garden", Label: "Garden", Pages: []string{"garden"}, First: true}},
		Pages: fstest.MapFS{"garden.html": {Data: []byte(`{{template "head" .}}<p>{{shout .Plant}}</p>{{template "foot" .}}`)}},
		Funcs: map[string]any{"shout": strings.ToUpper}}
	s.Mount = func(mux *http.ServeMux) {
		mux.HandleFunc("GET /garden", func(w http.ResponseWriter, r *http.Request) {
			s.Render(w, "garden", map[string]any{"Plant": "basil"})
		})
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/garden")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	html := string(body)
	if res.StatusCode != 200 || !strings.Contains(html, "<p>BASIL</p>") || !strings.Contains(html, `<a href="/garden" class="on"`) {
		t.Fatalf("%d\n%s", res.StatusCode, html)
	}
	if strings.Index(html, "/garden") > strings.Index(html, `href="/jobs"`) {
		t.Fatal("a First link should come right after Live")
	}
}

// A deployment's Home takes "/": the turns table moves to /turns, the Live
// link follows it, and an old link with a query still reaches the table.
func TestHome(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	a := &agent.Agent{Cfg: &config.Config{Name: "Exo", Loc: time.UTC}, Store: st}
	get := func(s *Server, path string) (int, string, string) {
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, res.Header.Get("Location"), string(body)
	}
	plain := &Server{Agent: a, Store: st}
	if code, _, body := get(plain, "/"); code != 200 || !strings.Contains(body, `<a href="/" class="on">Live</a>`) {
		t.Fatalf("without Home, / is the turns table: %d\n%s", code, body)
	}
	home := &Server{Agent: a, Store: st, Home: "/front"}
	if code, loc, _ := get(home, "/"); code != http.StatusFound || loc != "/front" {
		t.Fatalf("/ should redirect to Home: %d %q", code, loc)
	}
	if code, _, body := get(home, "/turns"); code != 200 || !strings.Contains(body, `<a href="/turns" class="on">Live</a>`) {
		t.Fatalf("/turns should be the turns table with its Live link: %d\n%s", code, body)
	}
	if code, _, body := get(home, "/?errors=1"); code != 200 || !strings.Contains(body, `id="turns"`) {
		t.Fatalf("an old /?query link should still get the table: %d\n%s", code, body)
	}
}
