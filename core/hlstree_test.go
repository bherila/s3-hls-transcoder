package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const rungPlaylist = "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\nseg_00000.m4s\n#EXTINF:2.0,\nseg_00001.m4s\n#EXT-X-ENDLIST\n"

// With several renditions ffmpeg names inits init_<index>.mp4.
const indexedRungPlaylist = "#EXTM3U\n#EXT-X-MAP:URI=\"init_1.mp4\"\n#EXTINF:6.0,\nseg_00000.m4s\n#EXTINF:2.0,\nseg_00001.m4s\n#EXT-X-ENDLIST\n"

func TestVersionHLSTreeRenamesMediaAndRewritesPlaylists(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"master.m3u8":        "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=360x640\n360p/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2,RESOLUTION=480x854\n480p/index.m3u8\n",
		"360p/index.m3u8":    rungPlaylist,
		"360p/init.mp4":      "i",
		"360p/seg_00000.m4s": "s0",
		"360p/seg_00001.m4s": "s1",
		"480p/index.m3u8":    indexedRungPlaylist,
		"480p/init_1.mp4":    "i",
		"480p/seg_00000.m4s": "s0",
		"480p/seg_00001.m4s": "s1",
	})

	if err := versionHLSTree(dir, "0-2-0-abcd1234"); err != nil {
		t.Fatal(err)
	}
	if err := validateHLSTree(dir); err != nil {
		t.Fatalf("renamed tree should validate: %v", err)
	}

	for _, rel := range []string{"360p/init_0-2-0-abcd1234.mp4", "480p/init_0-2-0-abcd1234.mp4", "480p/seg_0-2-0-abcd1234_00001.m4s"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	for _, rel := range []string{"360p/init.mp4", "360p/seg_00000.m4s"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			t.Errorf("%s should have been renamed", rel)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "360p/index.m3u8"))
	want := "#EXTM3U\n#EXT-X-MAP:URI=\"init_0-2-0-abcd1234.mp4\"\n#EXTINF:6.0,\nseg_0-2-0-abcd1234_00000.m4s\n#EXTINF:2.0,\nseg_0-2-0-abcd1234_00001.m4s\n#EXT-X-ENDLIST\n"
	if string(got) != want {
		t.Errorf("rewritten playlist:\n%s\nwant:\n%s", got, want)
	}
}

func TestValidateHLSTreeRejectsUnpublishableOutput(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n360p/index.m3u8\n"
	cases := map[string]map[string]string{
		// What ffmpeg wrote for a single rendition with init_<tag>_%v.mp4.
		"literal %v init": {
			"master.m3u8":        master,
			"360p/index.m3u8":    "#EXTM3U\n#EXT-X-MAP:URI=\"init_x_%v.mp4\"\n#EXTINF:6,\nseg_00000.m4s\n",
			"360p/init_x_%v.mp4": "i",
			"360p/seg_00000.m4s": "s",
		},
		"missing segment": {
			"master.m3u8":     master,
			"360p/index.m3u8": "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nseg_00000.m4s\n",
			"360p/init.mp4":   "i",
		},
		"escapes the tree": {
			"master.m3u8":     master,
			"360p/index.m3u8": "#EXTM3U\n#EXTINF:6,\n../../secret.m4s\n",
		},
		"missing rendition playlist": {
			"master.m3u8": master,
		},
		"no renditions": {
			"master.m3u8": "#EXTM3U\n",
		},
		// A zero-frame encode: an init map but no segment to play.
		"init but no segments": {
			"master.m3u8":     master,
			"360p/index.m3u8": "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-ENDLIST\n",
			"360p/init.mp4":   "i",
		},
	}
	for name, files := range cases {
		if err := validateHLSTree(writeTree(t, files)); err == nil {
			t.Errorf("%s: expected validation to fail", name)
		}
	}
}

func TestVersionHLSTreeRejectsUnsafeTags(t *testing.T) {
	dir := writeTree(t, map[string]string{"360p/index.m3u8": rungPlaylist})
	for _, tag := range []string{"", "a/b", "x%v", "../up"} {
		if err := versionHLSTree(dir, tag); err == nil || !strings.Contains(err.Error(), "unsafe media tag") {
			t.Errorf("tag %q: got %v", tag, err)
		}
	}
}

func TestVersionHLSTreeFollowsNestedRenditions(t *testing.T) {
	// HLS_LADDER rung names may contain "/", which nests the rendition directory.
	dir := writeTree(t, map[string]string{
		"master.m3u8":               "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nmobile/360p/index.m3u8\n",
		"mobile/360p/index.m3u8":    rungPlaylist,
		"mobile/360p/init.mp4":      "i",
		"mobile/360p/seg_00000.m4s": "s0",
		"mobile/360p/seg_00001.m4s": "s1",
	})
	if err := versionHLSTree(dir, "0-2-0-abcd1234"); err != nil {
		t.Fatal(err)
	}
	if err := validateHLSTree(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mobile/360p/init_0-2-0-abcd1234.mp4")); err != nil {
		t.Error(err)
	}
}
