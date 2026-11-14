package agent

import (
	"strings"
	"testing"

	"github.com/nathaniasSC2/exoskeleton/config"
)

// Send is the send_message tool: it reaches the person on a text channel, says
// where it landed, and leaves the message in the conversation as a notice.
func TestSend(t *testing.T) {
	a, p := testAgent(t)
	snd := &fakeSender{}
	a.AddChannel(Channel{Name: "sms", Sender: snd,
		ChatFor: func(p *config.Person) (string, error) { return p.Handles["sms"][0], nil }})

	where, err := a.Send(p.ID, "sms", "the link: https://example.com", nil)
	if err != nil || !strings.HasPrefix(where, "delivered to ") {
		t.Fatalf("Send = %q, %v", where, err)
	}
	if len(snd.sent) != 1 || snd.sent[0] != "+15550100: the link: https://example.com" {
		t.Fatalf("sent = %q", snd.sent)
	}
	rec, _ := a.Store.Recent(p.ID, 10)
	found := false
	for _, r := range rec {
		found = found || (r.Kind == "notice" && strings.Contains(r.Reply, "example.com"))
	}
	if !found {
		t.Fatal("the sent message isn't in the conversation")
	}

	if _, err := a.Send(p.ID, "", "x", nil); err == nil {
		t.Fatal("no Discord channel here, so the default should fail")
	}
	if _, err := a.Send(p.ID, "pager", "x", nil); err == nil {
		t.Fatal("unknown channel should fail")
	}
	if _, err := a.Send(p.ID, "sms", "  ", nil); err == nil {
		t.Fatal("an empty message should fail")
	}
	if len(snd.sent) != 1 {
		t.Fatalf("failed sends still went out: %q", snd.sent)
	}
}
