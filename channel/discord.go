package channel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Discord is DM-only: allowed people message the assistant directly. Guild traffic is
// ignored entirely, which is the simplest way to keep it a private console.
type Discord struct {
	InboxDir  string
	OnMessage func(Inbound)
	Allowed   func(userID string) bool

	s     *discordgo.Session
	ok    atomic.Bool
	mu    sync.Mutex
	dmFor map[string]string // user ID -> DM channel ID
}

func NewDiscord(token, inbox string, allowed func(string) bool, on func(Inbound)) (*Discord, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsDirectMessages | discordgo.IntentsDirectMessageReactions | discordgo.IntentMessageContent
	d := &Discord{InboxDir: inbox, OnMessage: on, Allowed: allowed, s: s, dmFor: map[string]string{}}
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Ready) { d.ok.Store(true); slog.Info("discord ready") })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { d.ok.Store(false) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) { d.ok.Store(true) })
	s.AddHandler(d.onCreate)
	s.AddHandler(d.onReact)
	return d, nil
}

func (d *Discord) Open() error   { return d.s.Open() } // discordgo reconnects and resumes on its own
func (d *Discord) Close()        { d.s.Close() }
func (d *Discord) Name() string  { return "discord" }
func (d *Discord) Healthy() bool { return d.ok.Load() }

func (d *Discord) onCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || m.GuildID != "" || !d.Allowed(m.Author.ID) {
		return
	}
	in := Inbound{Channel: "discord", Chat: m.ChannelID, Sender: m.Author.ID, GUID: "dc:" + m.ID,
		Text: strings.TrimSpace(m.Content), TS: m.Timestamp}
	for _, a := range m.Attachments {
		if IsAudio(a.ContentType, a.Filename) { // Discord voice messages are .ogg
			p, err := d.fetch(a.URL, a.ID+filepath.Ext(a.Filename))
			var t string
			if err == nil {
				t, err = Transcribe(p)
			}
			if err != nil {
				slog.Warn("voice message", "err", err)
			}
			in.Text = strings.TrimSpace(in.Text + "\n" + VoiceText(t, err))
			continue
		}
		if !strings.HasPrefix(a.ContentType, "image/") {
			in.Text = strings.TrimSpace(in.Text + fmt.Sprintf("\n[attachment not shown: %s (%s)]", a.Filename, a.ContentType))
			continue
		}
		if p, err := d.download(a.URL, a.ID+filepath.Ext(a.Filename)); err == nil {
			in.Images = append(in.Images, p)
		} else {
			slog.Warn("discord attachment", "err", err)
		}
	}
	if in.Text == "" && len(in.Images) == 0 {
		return
	}
	go d.OnMessage(in)
}

func (d *Discord) onReact(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if r.GuildID != "" || !d.Allowed(r.UserID) || s.State.User == nil || r.UserID == s.State.User.ID {
		return
	}
	score := 0
	switch r.Emoji.Name {
	case "👍", "❤️", "😂", "🔥", "💯":
		score = 1
	case "👎", "❓", "🤨", "💀":
		score = -1
	}
	if score != 0 {
		go d.OnMessage(Inbound{Channel: "discord", Chat: r.ChannelID, Sender: r.UserID, ReactTo: "dc:" + r.MessageID, Reaction: score, TS: time.Now()})
	}
}

func (d *Discord) download(u, name string) (string, error) {
	p, err := d.fetch(u, name)
	if err != nil {
		return "", err
	}
	return NormalizeImage(p)
}

// fetch saves an attachment as-is into the inbox.
func (d *Discord) fetch(u, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	os.MkdirAll(d.InboxDir, 0o755)
	p := filepath.Join(d.InboxDir, "dc_"+safeName(name))
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, 40<<20))
	f.Close()
	if err != nil {
		return "", err
	}
	return p, nil
}

// DM resolves a user ID to their DM channel (for unsolicited messages).
func (d *Discord) DM(userID string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.dmFor[userID]; ok {
		return c, nil
	}
	ch, err := d.s.UserChannelCreate(userID)
	if err != nil {
		return "", err
	}
	d.dmFor[userID] = ch.ID
	return ch.ID, nil
}

func (d *Discord) SendText(ctx context.Context, chat, text string) (string, error) {
	var last string
	for _, part := range packDiscord(text, 1900) {
		m, err := d.s.ChannelMessageSend(chat, part, discordgo.WithContext(ctx))
		if err != nil {
			return last, err
		}
		last = "dc:" + m.ID
	}
	return last, nil
}

func (d *Discord) SendFile(ctx context.Context, chat, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	m, err := d.s.ChannelFileSend(chat, filepath.Base(path), f, discordgo.WithContext(ctx))
	if err != nil {
		return "", err
	}
	return "dc:" + m.ID, nil
}

func (d *Discord) Typing(ctx context.Context, chat string, on bool) {
	if on {
		d.s.ChannelTyping(chat, discordgo.WithContext(ctx))
	}
}

// packDiscord keeps paragraphs together in as few messages as fit.
func packDiscord(text string, max int) []string {
	var out []string
	cur := ""
	for _, b := range SplitBubbles(text, max) {
		if cur != "" && len(cur)+2+len(b) > max {
			out = append(out, cur)
			cur = ""
		}
		if cur != "" {
			cur += "\n\n"
		}
		cur += b
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
