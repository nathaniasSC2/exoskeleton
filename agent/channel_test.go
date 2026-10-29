package agent

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nathaniasSC2/exoskeleton/channel"
	"github.com/nathaniasSC2/exoskeleton/config"
	"github.com/nathaniasSC2/exoskeleton/store"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeSender) Name() string  { return "sms" }
func (f *fakeSender) Healthy() bool { return true }
func (f *fakeSender) SendText(_ context.Context, chat, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, chat+": "+text)
	return "", nil
}
func (f *fakeSender) SendFile(context.Context, string, string) (string, error) { return "", nil }
func (f *fakeSender) Typing(context.Context, string, bool)                     {}

func testAgent(t *testing.T) (*Agent, *config.Person) {
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB.Close() })
	p := &config.Person{ID: "alex", Name: "Alex", Admin: true, HomeChat: "sms",
		Handles: map[string][]string{"sms": {"+15550100"}}}
	cfg := &config.Config{Name: "Exo", DataDir: t.TempDir(), Loc: time.UTC, People: []*config.Person{p}}
	a := &Agent{Cfg: cfg, Store: st}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	return a, p
}

func TestAddChannel(t *testing.T) {
	a, p := testAgent(t)
	snd := &fakeSender{}
	a.AddChannel(Channel{Name: "sms", Label: "a text message", Sender: snd,
		Who:     func(in channel.Inbound) *config.Person { return a.Cfg.ByHandle("sms", in.Sender) },
		ChatFor: func(p *config.Person) (string, error) { return p.Handles["sms"][0], nil }})
	if channelName("sms") != "a text message" || a.Senders["sms"] != snd {
		t.Fatal("channel not registered")
	}

	a.Handle(channel.Inbound{Channel: "sms", Chat: "c1", Sender: "+15550100", GUID: "g1", Text: "hi", TS: time.Now()})
	a.Handle(channel.Inbound{Channel: "sms", Chat: "c2", Sender: "+15550199", GUID: "g2", Text: "spam", TS: time.Now()})
	evs, _ := a.Store.PendingEvents(p.ID, time.Hour)
	if len(evs) != 1 || evs[0].Text != "hi" {
		t.Fatalf("pending events = %+v", evs)
	}

	if err := a.DeliverTarget("sms:alex", "report"); err != nil {
		t.Fatal(err)
	}
	if err := a.Ping("sms:alex", "a link"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverTarget("pager:alex", "x"); err == nil {
		t.Fatal("unknown channel should fail")
	}
	if len(snd.sent) != 2 || snd.sent[0] != "+15550100: report" {
		t.Fatalf("sent = %q", snd.sent)
	}
	// The ping is delivered but stays out of the conversation.
	rec, _ := a.Store.Recent(p.ID, 10)
	for _, r := range rec {
		if strings.Contains(r.Reply, "a link") {
			t.Fatal("a ping showed up in the recent conversation")
		}
	}
	if ch, chat := a.LastChat(p); ch != "sms" || chat != "c1" {
		t.Fatalf("last chat = %s %s", ch, chat)
	}
}

func TestAddPrompts(t *testing.T) {
	defer func(old []fs.FS) { promptFS = old }(promptFS)
	AddPrompts(fstest.MapFS{
		"rules.md":  {Data: []byte("house rules for {{name}}")},
		"extra.md":  {Data: []byte("an extra prompt")},
		"notes.txt": {Data: []byte("not a prompt")},
	})
	a, _ := testAgent(t)
	names := strings.Join(PromptNames(), ",")
	if !strings.Contains(names, "extra.md") || !strings.Contains(names, "soul.md") || strings.Contains(names, "notes.txt") {
		t.Fatalf("names = %s", names)
	}
	if got := a.ReadPrompt("rules.md"); got != "house rules for Exo" {
		t.Fatalf("rules = %q", got)
	}
}
