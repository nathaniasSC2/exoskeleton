// Package channel holds the messaging adapters. Each one turns platform
// traffic into an Inbound value and knows how to send text and files back.
// Platform metadata never reaches the model as text.
package channel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

type Inbound struct {
	Channel  string // "discord" | "console" | any adapter's name
	Chat     string // where replies go
	Sender   string // handle or user ID
	GUID     string // platform message ID, used for dedupe
	Text     string
	Images   []string // local file paths, already normalized to JPEG
	ReactTo  string   // set for tapbacks/reactions: the message reacted to
	Reaction int      // +1 / -1 for feedback reactions
	TS       time.Time
}

type Sender interface {
	Name() string
	SendText(ctx context.Context, chat, text string) (guid string, err error)
	SendFile(ctx context.Context, chat, path string) (guid string, err error)
	Typing(ctx context.Context, chat string, on bool)
	Healthy() bool
}

// Reader is a sender that can show the person their message was read.
type Reader interface {
	MarkRead(ctx context.Context, chat string)
}

// DryRun wraps a sender and logs instead of delivering. exo starts in dry
// run so a new setup can be exercised without anyone getting replies.
type DryRun struct {
	Inner Sender
	n     atomic.Int64
	Log   func(channel, chat, what string)
}

func (d *DryRun) Name() string  { return d.Inner.Name() }
func (d *DryRun) Healthy() bool { return d.Inner.Healthy() }
func (d *DryRun) SendText(_ context.Context, chat, text string) (string, error) {
	if d.Log != nil {
		d.Log(d.Inner.Name(), chat, text)
	}
	slog.Info("dry-run send", "channel", d.Inner.Name(), "chat", chat, "chars", len(text))
	return fmt.Sprintf("dry-%d", d.n.Add(1)), nil
}
func (d *DryRun) SendFile(_ context.Context, chat, path string) (string, error) {
	if d.Log != nil {
		d.Log(d.Inner.Name(), chat, "[file] "+path)
	}
	return fmt.Sprintf("dry-%d", d.n.Add(1)), nil
}
func (d *DryRun) Typing(context.Context, string, bool) {}

// Console is the dashboard's test channel: replies only land in the database.
type Console struct{}

func (Console) Name() string                                             { return "console" }
func (Console) Healthy() bool                                            { return true }
func (Console) SendText(context.Context, string, string) (string, error) { return "", nil }
func (Console) SendFile(context.Context, string, string) (string, error) { return "", nil }
func (Console) Typing(context.Context, string, bool)                     {}

// NormalizeImage converts whatever arrived (HEIC from iPhones, huge PNGs) into
// a JPEG no larger than 1600px on the long edge, which keeps the image tokens
// sane and is a format every model accepts.
func NormalizeImage(src string) (string, error) {
	dst := strings.TrimSuffix(src, filepath.Ext(src)) + ".exo.jpg"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "convert", src+"[0]", "-auto-orient", "-resize", "1600x1600>",
		"-quality", "85", dst).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("convert: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if src != dst {
		os.Remove(src)
	}
	return dst, nil
}

// SplitBubbles breaks a reply into message-sized pieces at paragraph breaks.
func SplitBubbles(text string, max int) []string {
	var out []string
	for _, p := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		p = strings.TrimSpace(p)
		for len(p) > max {
			cut := strings.LastIndexAny(p[:max], "\n.!? ")
			if cut < max/2 {
				cut = max
			}
			out = append(out, strings.TrimSpace(p[:cut+boolInt(cut < max)]))
			p = strings.TrimSpace(p[cut+boolInt(cut < max):])
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Transcriber is the command that turns an audio file into text (it gets the
// path as its last argument and prints the transcript), e.g. a faster-whisper
// script. Set from EXO_TRANSCRIBE at startup; empty = voice memos aren't transcribed.
var Transcriber []string

// Transcribe turns a voice memo into text. The audio file is kept for the trace.
func Transcribe(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if len(Transcriber) == 0 {
		return "", fmt.Errorf("no transcriber configured")
	}
	cmd := exec.CommandContext(ctx, Transcriber[0], append(Transcriber[1:], path)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("transcribe: %v: %s", err, lastLine(string(ee.Stderr)))
		}
		return "", fmt.Errorf("transcribe: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// safeName keeps a file name to letters, digits, - and _.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// IsAudio reports whether an attachment is a voice memo or other audio.
func IsAudio(mime, name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(mime, "audio/") || strings.HasSuffix(n, ".caf") || strings.HasSuffix(n, ".m4a") ||
		strings.HasSuffix(n, ".ogg") || strings.HasSuffix(n, ".opus") || strings.HasSuffix(n, ".mp3") || strings.HasSuffix(n, ".amr")
}

// voiceText is how a memo shows up in the conversation.
func voiceText(transcript string, err error) string {
	if err != nil {
		return "[sent a voice memo, but it couldn't be transcribed]"
	}
	if transcript == "" {
		return "[sent a voice memo with no audible speech]"
	}
	return "[voice memo] " + transcript
}
