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
