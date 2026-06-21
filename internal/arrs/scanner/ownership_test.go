package scanner

import "testing"

func TestPathContainsDir(t *testing.T) {
	tests := []struct {
		name string
		p    string
		dir  string
		want bool
	}{
		{"exact match", "/tv/Show", "/tv/Show", true},
		{"child path", "/tv/Show/Season 01/ep.mkv", "/tv/Show", true},
		{"trailing slash on dir still matches", "/tv/Show/Season 01/ep.mkv", "/tv/Show/", true},
		{"trailing slash on p", "/tv/Show/", "/tv/Show", true},
		{"sibling rejected", "/tv/Showtime/ep.mkv", "/tv/Show", false},
		{"cross-prefix child", "/downloads/tv/Show/ep.mkv", "/tv/Show", true},
		{"no match", "/tv/Other/ep.mkv", "/tv/Show", false},
		{"empty dir", "/tv/Show/ep.mkv", "", false},
		{"root-only dir", "/tv/Show/ep.mkv", "/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathContainsDir(tt.p, tt.dir); got != tt.want {
				t.Errorf("pathContainsDir(%q, %q) = %v; want %v", tt.p, tt.dir, got, tt.want)
			}
		})
	}
}
