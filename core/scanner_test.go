package core

import "testing"

func TestIsVideoKey(t *testing.T) {
	video := []string{"a.mp4", "clip.mov", "clip.mkv", "clip.webm", "clip.avi", "clip.m4v", "a/b/c.mov", "video.MKV", "Video.Mp4", "v1.0/raw.mp4"}
	for _, k := range video {
		if !IsVideoKey(k) {
			t.Errorf("IsVideoKey(%q) = false, want true", k)
		}
	}
	notVideo := []string{"notes.txt", "image.png", "audio.mp3", "doc.pdf", "video", "a/b/file", "v1.0/raw"}
	for _, k := range notVideo {
		if IsVideoKey(k) {
			t.Errorf("IsVideoKey(%q) = true, want false", k)
		}
	}
}
