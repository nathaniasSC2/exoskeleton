package web

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nathaniasSC2/exoskeleton/channel"
)

// upload reads the console form: its text, plus an optional image (normalized
// to JPEG) or audio file (transcribed, like a voice memo from a channel).
func (s *Server) upload(r *http.Request, prefix string) (string, []string) {
	text := strings.TrimSpace(r.FormValue("text"))
	var images []string
	f, hdr, err := r.FormFile("image")
	if err != nil {
		return text, nil
	}
	defer f.Close()
	dir := s.Agent.Cfg.Path("inbox")
	os.MkdirAll(dir, 0o755)
	ext := filepath.Ext(hdr.Filename)
	if ext == "" {
		ext = extFor(hdr.Header.Get("Content-Type"))
	}
	p := filepath.Join(dir, fmt.Sprintf("%s-%d%s", prefix, time.Now().UnixNano(), ext))
	out, err := os.Create(p)
	if err != nil {
		return text, nil
	}
	io.Copy(out, io.LimitReader(f, 25<<20))
	out.Close()
	if channel.IsAudio(hdr.Header.Get("Content-Type"), hdr.Filename) {
		t, err := channel.Transcribe(p)
		memo := "[voice memo] " + t
		if err != nil || t == "" {
			memo = "[sent a voice memo, but it couldn't be transcribed]"
		}
		text = strings.TrimSpace(text + "\n" + memo)
	} else if jpg, err := channel.NormalizeImage(p); err == nil {
		images = append(images, jpg)
	}
	return text, images
}

func extFor(mime string) string {
	mime, _, _ = strings.Cut(mime, ";")
	switch mime {
	case "audio/mp4", "audio/x-m4a", "audio/aac":
		return ".m4a"
	case "audio/webm":
		return ".webm"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "image/png":
		return ".png"
	case "image/heic":
		return ".heic"
	}
	return ".jpg"
}
