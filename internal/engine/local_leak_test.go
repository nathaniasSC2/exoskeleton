package engine

import "testing"

// Turn 329 of the 2026-10-13 trial: the self-check rewrite thought aloud and
// said the answer twice.
func TestStripScratchTrial329(t *testing.T) {
	in := "Wait, I need to check what actually hosts/musical guest is on Oct 31. Let me fix that:\n\n" +
		"No SNL this weekend — they skipped Oct 17 and 24. Next one up is Halloween night Oct 31 with host Indie/Ariana Grande as musical guest. Then midseason break after that.\n\n" +
		"Hmm, the first result got cut off. Let me just say what I know for sure:\n\n" +
		"No SNL this weekend — they skipped Oct 17 and 24. Next episode is Halloween night Oct 31 (host TBD with Ariana Grande). Then midseason break."
	want := "No SNL this weekend — they skipped Oct 17 and 24. Next episode is Halloween night Oct 31 (host TBD with Ariana Grande). Then midseason break."
	if got := stripScratch(in); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestStripScratchLeavesCleanReplies(t *testing.T) {
	in := "Yep — Lions @ Patriots, Sunday Oct 18, 1:00 PM EDT.\n\nInjury reports drop Wed–Fri."
	if got := stripScratch(in); got != in {
		t.Fatalf("changed a clean reply: %q", got)
	}
}

func TestLeakedCheckDetection(t *testing.T) {
	leaky := []string{
		"I just made that $40–$60 range up and shouldn't have — I didn't look anything up. In all seriousness: ...",
		"I need to correct myself — both were guesses.",
		"Wait, let me fix that.\n\nNo SNL this weekend.",
	}
	for _, s := range leaky {
		if !confessRe.MatchString(s) && !scratchRe.MatchString(s) {
			t.Errorf("missed leak: %q", s)
		}
	}
	clean := []string{
		"No SNL this weekend. Back Oct 31.",
		"I couldn't check the injury report, so I don't know.",
		"Actually, the game is at 1 PM.",
	}
	for _, s := range clean {
		if confessRe.MatchString(s) || scratchRe.MatchString(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}

// Turns 362-364 of the 2026-10-14 9B trial gave up without trying a tool.
func TestGiveUpDetection(t *testing.T) {
	gaveUp := []string{
		"I can't see any Garrett Wilson TD photo you're referring to. I need you to send me a link or upload the file for me to work with it.",
		"The only way to find that specific image would be to share a link you already found, or send me the file directly.",
		"I'm unable to look that up right now.",
	}
	for _, s := range gaveUp {
		if !giveUpRe.MatchString(s) {
			t.Errorf("missed give-up: %q", s)
		}
	}
	fine := []string{
		"Lions @ Patriots, Sunday Oct 18, 1:00 PM ET.",
		"Here's the wallpaper — I cropped it to your phone's size.",
		"Can you tell me which play you mean, the first TD or the second?",
	}
	for _, s := range fine {
		if giveUpRe.MatchString(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}
