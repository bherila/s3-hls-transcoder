package core

import "testing"

func TestIsImageKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"uploads/0/photo.jpg", true},
		{"uploads/0/photo.JPEG", true},
		{"a/b/c.png", true},
		{"x.webp", true},
		{"x.gif", true},
		{"x.avif", true},
		{"x.heic", true},
		{"clip.mp4", false},  // video, not image
		{"notes.txt", false}, // other
		{"noext", false},
		{"trailing/", false},
	}
	for _, c := range cases {
		if got := IsImageKey(c.key); got != c.want {
			t.Errorf("IsImageKey(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

func TestImageKeyAndVideoKeyAreDisjoint(t *testing.T) {
	// A key is never both an image and a video; guards against an extension
	// landing in both maps.
	for ext := range imageExtensions {
		if videoExtensions[ext] {
			t.Errorf("extension %q is in both imageExtensions and videoExtensions", ext)
		}
	}
}

func TestImageMappingKey(t *testing.T) {
	if got := ImageMappingKey("uploads/0/x.jpg"); got != "image-mappings/uploads/0/x.jpg.json" {
		t.Errorf("ImageMappingKey = %q", got)
	}
}

func TestIsCachedImageMapping(t *testing.T) {
	m := &ImageMapping{SourceEtag: "abc", SourceSize: 100}
	if !IsCachedImageMapping(m, "abc", 100) {
		t.Error("expected cache hit for matching etag+size")
	}
	if IsCachedImageMapping(m, "abc", 101) {
		t.Error("size mismatch should not be a cache hit")
	}
	if IsCachedImageMapping(m, "xyz", 100) {
		t.Error("etag mismatch should not be a cache hit")
	}
	if IsCachedImageMapping(nil, "abc", 100) {
		t.Error("nil mapping should not be a cache hit")
	}
}
