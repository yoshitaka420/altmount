package scanner

import (
	"path/filepath"
	"strings"
)

// pathContainsDir reports whether dir occurs in p terminated at a path-component
// boundary (the next character is a separator or end of string). This keeps the
// original lenient "appears anywhere" behavior across differing path prefixes
// while preventing a sibling directory from matching by raw substring (e.g.
// "/tv/Show" must not match "/tv/Showtime/...").
func pathContainsDir(p, dir string) bool {
	p = filepath.ToSlash(p)
	// Trim any trailing separators so the component-boundary check below lines up
	// with the end of the matched directory (e.g. dir "/tv/Show/" must still
	// match "/tv/Show/ep.mkv").
	dir = strings.TrimRight(filepath.ToSlash(dir), "/")
	if dir == "" {
		return false
	}
	for i := 0; ; {
		idx := strings.Index(p[i:], dir)
		if idx < 0 {
			return false
		}
		end := i + idx + len(dir)
		if end == len(p) || p[end] == '/' {
			return true
		}
		i = i + idx + 1
	}
}
