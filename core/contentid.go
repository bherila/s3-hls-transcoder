package core

import (
	"fmt"
	"strings"
)

// ContentIDScheme identifies how a content ID was derived. v1 emits only
// "sha256"; "psig" is reserved for a future perceptual-signature scheme.
type ContentIDScheme string

const (
	SchemeSHA256 ContentIDScheme = "sha256"
	SchemePSig   ContentIDScheme = "psig"
)

var knownSchemes = map[ContentIDScheme]bool{SchemeSHA256: true, SchemePSig: true}

// FormatContentID joins a scheme and id into a scheme-prefixed content ID,
// e.g. "sha256:f7c3...".
func FormatContentID(scheme ContentIDScheme, id string) string {
	return string(scheme) + ":" + id
}

// ParseContentID splits a scheme-prefixed content ID. It errors on a missing
// prefix or an unknown scheme.
func ParseContentID(contentID string) (ContentIDScheme, string, error) {
	colon := strings.IndexByte(contentID, ':')
	if colon == -1 {
		return "", "", fmt.Errorf("content ID missing scheme prefix: %s", contentID)
	}
	scheme := ContentIDScheme(contentID[:colon])
	id := contentID[colon+1:]
	if !knownSchemes[scheme] {
		return "", "", fmt.Errorf("unknown content ID scheme: %s", scheme)
	}
	return scheme, id, nil
}

// ByIDPrefix is the destination-bucket key prefix for a content ID's output.
func ByIDPrefix(contentID string) string { return "by-id/" + contentID + "/" }

// MasterPlaylistKey is the canonical master playlist key for a content ID.
func MasterPlaylistKey(contentID string) string { return ByIDPrefix(contentID) + "master.m3u8" }

// MetadataKey is the metadata.json key for a content ID.
func MetadataKey(contentID string) string { return ByIDPrefix(contentID) + "metadata.json" }
