package core

import (
	"strings"
	"time"
)

// SourceObject is a candidate source video listed from the source bucket.
type SourceObject struct {
	Key          string
	ETag         string
	Size         int64
	LastModified time.Time
}

// ScanOptions controls source-bucket scanning.
type ScanOptions struct {
	Prefix string
	// Filter overrides the default video-extension filter. Used by the cleanup
	// pass to enumerate every key.
	Filter func(key string) bool
}

var videoExtensions = map[string]bool{
	"mp4": true, "mov": true, "mkv": true, "webm": true, "avi": true,
	"m4v": true, "mpg": true, "mpeg": true, "wmv": true, "flv": true,
	"ogv": true, "3gp": true, "ts": true, "m2ts": true,
}

var imageExtensions = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "webp": true, "gif": true,
	"bmp": true, "tif": true, "tiff": true, "avif": true, "heic": true, "heif": true,
}

// IsVideoKey reports whether a key's basename has a recognized video extension.
func IsVideoKey(key string) bool {
	return hasExtension(key, videoExtensions)
}

// IsImageKey reports whether a key's basename has a recognized image extension.
func IsImageKey(key string) bool {
	return hasExtension(key, imageExtensions)
}

func hasExtension(key string, set map[string]bool) bool {
	name := key
	if slash := strings.LastIndexByte(key, '/'); slash != -1 {
		name = key[slash+1:]
	}
	dot := strings.LastIndexByte(name, '.')
	if dot == -1 {
		return false
	}
	return set[strings.ToLower(name[dot+1:])]
}
