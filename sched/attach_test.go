package sched

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAttachments(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.mp4")
	os.WriteFile(f, []byte("x"), 0o644)
	out := "Today's TikToks\nATTACH: " + f + "\n1. caption\nATTACH: relative.mp4\nATTACH: " + dir + "/missing.mp4\nATTACH: " + dir
	text, files := Attachments(out)
	if !reflect.DeepEqual(files, []string{f}) {
		t.Fatalf("files = %v", files)
	}
	want := "Today's TikToks\n1. caption\nATTACH: relative.mp4\nATTACH: " + dir + "/missing.mp4\nATTACH: " + dir
	if text != want {
		t.Fatalf("text = %q", text)
	}
	if text, files := Attachments("ATTACH: " + f); text != "" || len(files) != 1 {
		t.Fatalf("attach only: %q %v", text, files)
	}
	if text, files := Attachments("no files here"); text != "no files here" || files != nil {
		t.Fatalf("plain: %q %v", text, files)
	}
}
