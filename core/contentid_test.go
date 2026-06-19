package core

import (
	"strings"
	"testing"
)

func TestFormatParseContentID(t *testing.T) {
	id := FormatContentID(SchemeSHA256, "abc123")
	if id != "sha256:abc123" {
		t.Fatalf("FormatContentID = %q", id)
	}
	scheme, body, err := ParseContentID(id)
	if err != nil || scheme != SchemeSHA256 || body != "abc123" {
		t.Fatalf("ParseContentID = (%q,%q,%v)", scheme, body, err)
	}
	if s, b, err := ParseContentID("psig:xyz"); err != nil || s != SchemePSig || b != "xyz" {
		t.Fatalf("ParseContentID psig = (%q,%q,%v)", s, b, err)
	}
	if _, _, err := ParseContentID("abc123"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "missing scheme") {
		t.Fatalf("expected missing-scheme error, got %v", err)
	}
	if _, _, err := ParseContentID("md5:abc"); err == nil || !strings.Contains(err.Error(), "unknown content ID scheme") {
		t.Fatalf("expected unknown-scheme error, got %v", err)
	}
}

func TestCanonicalPaths(t *testing.T) {
	id := "sha256:abc"
	if got := ByIDPrefix(id); got != "by-id/sha256:abc/" {
		t.Errorf("ByIDPrefix = %q", got)
	}
	if got := MasterPlaylistKey(id); got != "by-id/sha256:abc/master.m3u8" {
		t.Errorf("MasterPlaylistKey = %q", got)
	}
	if got := MetadataKey(id); got != "by-id/sha256:abc/metadata.json" {
		t.Errorf("MetadataKey = %q", got)
	}
}
