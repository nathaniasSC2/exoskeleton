package agent

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nathaniasSC2/exoskeleton/engine"
	"github.com/nathaniasSC2/exoskeleton/store"
)

// TestBudgets holds exo's own default prompts to the budgets in budget.go.
// A deployment with its own prompts runs the same checks over them
// (CheckBudgets) in its own tests.
func TestBudgets(t *testing.T) {
	a, p := testAgent(t)
	for _, problem := range CheckBudgets(a, p) {
		t.Error(problem)
	}
}

// TestSystemPromptCapped: whatever the database holds, the dynamic parts of
// the system prompt stay inside their caps.
func TestSystemPromptCapped(t *testing.T) {
	a, p := testAgent(t)
	line := strings.Repeat("x", 150)
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("%d %s", i, line))
	}
	a.Store.Set("interests:"+p.ID, strings.Join(many, "\n"))
	a.Store.Set("loops:"+p.ID, strings.Join(many, "\n"))
	for i := 0; i < 30; i++ {
		id, _ := a.Store.AddFact(p.ID, fmt.Sprintf("fact %d %s", i, line), true, "test")
		_ = id
	}
	s, _ := a.systemPrompt(p)
	static := len(a.soul(p)) + len(a.prompt("rules.md")) + len(a.prompt("admin.md"))
	if dyn := len(s) - static; dyn > maxInterestsChars+maxLoopsChars+maxPinnedChars+300 {
		t.Fatalf("dynamic part of the system prompt is %d chars", dyn)
	}
}

func TestSetListsRefuseOverCap(t *testing.T) {
	a, p := testAgent(t)
	if err := a.SetInterests(p.ID, strings.Repeat("y", maxInterestsChars+1)); err == nil {
		t.Fatal("over-long interests list accepted")
	}
	if err := a.SetLoops(p.ID, strings.Repeat("y", maxLoopsChars+1)); err == nil {
		t.Fatal("over-long open loops accepted")
	}
	if err := a.SetLoops(p.ID, "a\nb"); err != nil || a.Store.Get("loops:"+p.ID) != "a\nb" {
		t.Fatalf("short loops: %v", err)
	}
}

func TestSavePromptRefusesOverBudget(t *testing.T) {
	a, _ := testAgent(t)
	if err := a.SavePrompt("heartbeat.md", strings.Repeat("z", PromptBudgets["heartbeat.md"]+1)); err == nil {
		t.Fatal("over-budget heartbeat.md saved")
	}
	if err := a.SavePrompt("heartbeat.md", "short brief"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(a.Cfg.Path("prompts", "heartbeat.md"), []byte(strings.Repeat("z", 7000)), 0o644) // edited on disk
	if !strings.Contains(a.OverBudget(), "heartbeat.md") {
		t.Fatal("maintenance should report a prompt edited past its budget")
	}
}

// TestChatContextBudget: a full history window plus every kind of retrieval,
// all oversized, still fits the channel's budget.
func TestChatContextBudget(t *testing.T) {
	a, p := testAgent(t)
	long := strings.Repeat("jets jellyfin gardenlog backup ", 60)
	for i := 0; i < 60; i++ {
		id := a.Store.StartTurn(p.ID, "sms", "c", "chat", long)
		a.Store.DB.Exec(`UPDATE turns SET status='ok', reply=?, tools=?, started=? WHERE id=?`, long, "notes_read, shell, shell, sports, WebSearch",
			time.Now().Add(-time.Duration(60-i)*time.Minute).Unix(), id)
	}
	old := a.Store.StartTurn(p.ID, "sms", "c", "chat", long)
	a.Store.DB.Exec(`UPDATE turns SET status='ok', reply=?, started=? WHERE id=?`, long, time.Now().Add(-200*time.Hour).Unix(), old)
	for i := 0; i < 20; i++ {
		a.Store.AddFact(p.ID, fmt.Sprintf("jets fact %d %s", i, long), false, "test")
		a.Store.AddReminder(&store.Reminder{Person: p.ID, Channel: "sms", Chat: "c", At: time.Now().Add(time.Hour).Unix(), Text: long})
		a.Store.DB.Exec(`INSERT INTO jobs(person,channel,chat,task,workdir,status,created) VALUES(?,?,?,?,?,?,?)`, p.ID, "sms", "c", long, "/tmp", "running", time.Now().Unix())
	}
	id := a.Store.StartTurn(p.ID, "sms", "c", "chat", "what about the jets and jellyfin")
	h := a.history(p, id, "sms")
	input := "what about the jets and jellyfin"
	user := a.contextBlock(p, "sms", input, "claude", id, false, capsFor("sms").Budget-len(engine.HistoryText(h)))
	got := len(engine.HistoryText(h)) + len(user) - len(input)
	if got > BudgetChatContext {
		t.Fatalf("history (%d) + context block = %d chars, budget %d", len(engine.HistoryText(h)), got, BudgetChatContext)
	}
	if !strings.Contains(user, "Now: ") || !strings.HasSuffix(user, input) {
		t.Fatalf("context block lost its fixed lines or the message:\n%s", user)
	}
	ph, pb := a.PreviewContext(p, "sms", input)
	if len(ph)+len(pb) > BudgetChatContext || strings.Contains(pb, input) {
		t.Fatalf("PreviewContext: %d + %d chars", len(ph), len(pb))
	}
}
