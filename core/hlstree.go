package core

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// After ffmpeg writes an HLS tree with its default names (init.mp4,
// seg_00000.m4s, …), versionHLSTree renames each rendition's media to this
// encode's unique names and rewrites the rendition playlist to match, and
// validateHLSTree checks the result before anything is uploaded. Naming is
// done here rather than through ffmpeg's -hls_* filename templates because
// those expand differently by rendition count (a single rendition writes a
// literal "%v"), and an unchecked tree would publish whatever came out.

var (
	defaultSegment = regexp.MustCompile(`^seg_(\d+)\.m4s$`)
	// ffmpeg's default init name is init.mp4 for one rendition but
	// init_<index>.mp4 when there are several.
	defaultInit = regexp.MustCompile(`^init(_\d+)?\.mp4$`)
	safeHLSPath = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
	uriAttr     = regexp.MustCompile(`URI="([^"]*)"`)
)

// versionHLSTree renames the init (init.mp4 or init_N.mp4) and seg_NNNNN.m4s
// of every rendition master.m3u8 names to init_<tag>.mp4 and
// seg_<tag>_NNNNN.m4s, rewriting each rendition playlist to the new names.
func versionHLSTree(dir, tag string) error {
	if !safeHLSPath.MatchString(tag) || strings.Contains(tag, "/") {
		return fmt.Errorf("unsafe media tag %q", tag)
	}
	// Follow the renditions the master playlist names: an HLS_LADDER rung
	// name may contain "/" (e.g. "mobile/360p"), nesting its directory.
	variants, _, err := playlistRefs(dir, "master.m3u8")
	if err != nil {
		return err
	}
	for _, v := range variants {
		if err := versionRendition(filepath.Join(dir, filepath.FromSlash(path.Dir(v))), path.Base(v), tag); err != nil {
			return fmt.Errorf("rendition %s: %w", v, err)
		}
	}
	return nil
}

func versionRendition(dir, playlistName, tag string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	renames := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		switch {
		case defaultInit.MatchString(name):
			renames[name] = "init_" + tag + ".mp4"
		case defaultSegment.MatchString(name):
			renames[name] = "seg_" + tag + "_" + defaultSegment.FindStringSubmatch(name)[1] + ".m4s"
		}
	}
	for from, to := range renames {
		if err := os.Rename(filepath.Join(dir, from), filepath.Join(dir, to)); err != nil {
			return err
		}
	}

	playlist := filepath.Join(dir, playlistName)
	raw, err := os.ReadFile(playlist)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			lines[i] = uriAttr.ReplaceAllStringFunc(line, func(m string) string {
				uri := uriAttr.FindStringSubmatch(m)[1]
				if to, ok := renames[uri]; ok {
					return `URI="` + to + `"`
				}
				return m
			})
		default:
			if to, ok := renames[trimmed]; ok {
				lines[i] = to
			}
		}
	}
	return os.WriteFile(playlist, []byte(strings.Join(lines, "\n")), 0o644)
}

// validateHLSTree checks that master.m3u8 and every playlist it reaches only
// reference safe relative paths to files that exist in the tree, so a bad
// ffmpeg layout fails the encode instead of being published.
func validateHLSTree(dir string) error {
	variants, _, err := playlistRefs(dir, "master.m3u8")
	if err != nil {
		return err
	}
	if len(variants) == 0 {
		return fmt.Errorf("master.m3u8 references no renditions")
	}
	for _, v := range variants {
		// Segments are the bare URI lines; an EXT-X-MAP init alone (e.g. a
		// zero-frame encode) is not playable.
		segments, _, err := playlistRefs(dir, v)
		if err != nil {
			return err
		}
		if len(segments) == 0 {
			return fmt.Errorf("%s references no segments", v)
		}
	}
	return nil
}

// playlistRefs returns the tree-relative paths a playlist references — bare
// URI lines (renditions in a master, segments in a rendition playlist) and URI
// attributes (e.g. EXT-X-MAP) separately — checking each is safe and exists.
func playlistRefs(dir, rel string) (lines, attrs []string, err error) {
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, nil, fmt.Errorf("playlist %s: %w", rel, err)
	}
	defer f.Close()

	base := path.Dir(rel)
	check := func(uri string) (string, error) {
		target := path.Clean(path.Join(base, uri))
		if !safeHLSPath.MatchString(uri) || strings.HasPrefix(target, "../") || target == ".." {
			return "", fmt.Errorf("%s references unsafe path %q", rel, uri)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(target))); err != nil {
			return "", fmt.Errorf("%s references missing %q", rel, uri)
		}
		return target, nil
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			for _, m := range uriAttr.FindAllStringSubmatch(line, -1) {
				target, err := check(m[1])
				if err != nil {
					return nil, nil, err
				}
				attrs = append(attrs, target)
			}
		default:
			target, err := check(line)
			if err != nil {
				return nil, nil, err
			}
			lines = append(lines, target)
		}
	}
	return lines, attrs, sc.Err()
}
