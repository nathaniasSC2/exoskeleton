package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nathaniasSC2/exoskeleton/internal/tools"
)

func TestClaimRe(t *testing.T) {
	claims := []string{"The render is done. I'll send it over in a sec.", "Still cooking, I'll ping you", "I've started the job",
		"It's running now", "I've set up a reminder for 5pm", "kicked off the render", "I'll start a new render with this picture",
		"I'll update job #6 to keep the funnel wider", "it'll ping you the second it's done", "The render is still chugging away", "Okay, I'm on it!",
		"I'm keeping an eye on job #9 in the background.", "For Job #6, I made sure the script generates a wider funnel",
		"I tweaked the camera so it stays tighter", "Job #3 started!", "Starting the render now with Lou's photo",
		"Alright, here's how it looks: clean green/gold text overlay, subtle rain texture, and the WT celebration cropped tight. Want to tweak anything or just grab it now?",
		"Here's your wallpaper!", "I made a lock screen version too", "I cropped it to your phone's size", "The wallpaper came out clean",
		"Done — reminder set for Thursday at 9am to run the Kortz Center heist setup.", "All set, see you Thursday", "Reminder's set for 9."}
	fine := []string{"Honey mustard, easily.", "Want me to check on it?", "I can't start renders on this brain.",
		"The Lions lost 24-17.", "Haha, enjoy the bath!", "Here's the schedule: Lions @ Patriots, Sunday 1 PM.",
		"Here's what I found on Getty.", "Want me to make it into a wallpaper?", "He made a great catch on the second TD.",
		"Done with work yet?", "Want me to set a reminder for Thursday?"}
	for _, s := range claims {
		if !claimRe.MatchString(s) {
			t.Errorf("should flag %q", s)
		}
	}
	for _, s := range fine {
		if claimRe.MatchString(s) {
			t.Errorf("should not flag %q", s)
		}
	}
}

func TestParseLeakedCalls(t *testing.T) {
	txt := "Sure.\n<tool_call>\n<function=toolbox>\n<parameter=tool>\nstart_job\n</parameter>\n<parameter=args>\n{\"task\": \"v4\"}\n</parameter>\n</function>\n</tool_call>"
	calls := parseLeakedCalls(txt, 0)
	if len(calls) != 1 || calls[0].Function.Name != "toolbox" || calls[0].Function.Arguments != `{"args":{"task":"v4"},"tool":"start_job"}` {
		t.Fatalf("%#v", calls)
	}
}

func TestRepairIFM(t *testing.T) {
	a := tools.Args{"tool": "shell\n<ifm|arg_key>command</ifm|arg_key>\n<ifm|arg_value>ls -lt /var/lib/ 2>/dev/null | head -20"}
	got, ok := repairIFM(a)
	if !ok || got["tool"] != "shell" || got["args"].(map[string]any)["command"] != "ls -lt /var/lib/ 2>/dev/null | head -20" {
		t.Fatalf("%#v", got)
	}
	a = tools.Args{"tool": "shell\n<ifm|arg_key>args</ifm|arg_key>\n<ifm|arg_value>{\"command\": \"df -h\"}"}
	if got, ok = repairIFM(a); !ok || got["args"].(map[string]any)["command"] != "df -h" {
		t.Fatalf("nested args: %#v", got)
	}
	if _, ok := repairIFM(tools.Args{"tool": "jobs"}); ok {
		t.Fatalf("a clean call must be left alone")
	}
	leak := "<ifm|tool_calls> <ifm|tool_call>toolbox <ifm|arg_key>tool</ifm|arg_key> <ifm|arg_value>jobs</ifm|arg_value> <ifm|arg_key>args</ifm|arg_key> <ifm|arg_value>{\"id\": 6}</ifm|arg_value> </ifm|tool_call>"
	calls := parseLeakedCalls(leak, 0)
	if len(calls) != 1 || calls[0].Function.Name != "toolbox" || calls[0].Function.Arguments != `{"args":{"id":6},"tool":"jobs"}` {
		t.Fatalf("leaked K2 call: %#v", calls)
	}
}

func TestLeakRe(t *testing.T) {
	for _, s := range []string{"On it. ctrl94{task=\"Render a dog\"}", "<tool_call>{...}", "<function=toolbox>", `{"tool": "start_job"}`} {
		if !leakRe.MatchString(s) {
			t.Errorf("should flag %q", s)
		}
	}
	for _, s := range []string{"The Lions lost 24-17.", "Use the {tool} you like", "control panel"} {
		if leakRe.MatchString(s) {
			t.Errorf("should not flag %q", s)
		}
	}
}

func TestFakeUsedTag(t *testing.T) {
	if got := fakeUsedRe.ReplaceAllString("(used: web_search) The starter is out.", ""); got != "The starter is out." {
		t.Fatalf("%q", got)
	}
}

func TestUserContentImages(t *testing.T) {
	if s, ok := userContent([]Block{{Text: "hi"}}, true).(string); !ok || s != "hi" {
		t.Fatalf("text only: %#v", s)
	}
	p := filepath.Join(t.TempDir(), "a.jpg")
	os.WriteFile(p, []byte{0xff, 0xd8, 0xff}, 0o644)
	if s, ok := userContent([]Block{{Text: "look"}, {Image: p}}, false).(string); !ok || !strings.Contains(s, "can't see images") {
		t.Fatalf("no vision: %#v", s)
	}
	parts, ok := userContent([]Block{{Text: "look"}, {Image: p}}, true).([]map[string]any)
	if !ok || len(parts) != 2 || parts[1]["type"] != "image_url" {
		t.Fatalf("with image: %#v", parts)
	}
}
