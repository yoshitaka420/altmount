package scanner

import "testing"

func TestParseSeasonEpisode(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantSeason  int
		wantEpisode int
		wantOK      bool
	}{
		{
			name:        "standard SxxExx",
			input:       "/media/Show/Season 01/Show.S01E05.1080p.mkv",
			wantSeason:  1,
			wantEpisode: 5,
			wantOK:      true,
		},
		{
			name:        "lowercase and high numbers",
			input:       "Show.s2024e0123.mkv",
			wantSeason:  2024,
			wantEpisode: 123,
			wantOK:      true,
		},
		{
			name:   "multi-episode E-style is ambiguous",
			input:  "Show.S01E01E02.1080p.mkv",
			wantOK: false,
		},
		{
			name:   "multi-episode dash-E style is ambiguous",
			input:  "Show.S01E01-E02.mkv",
			wantOK: false,
		},
		{
			name:   "multi-episode dash-number style is ambiguous",
			input:  "Show.S01E01-02.mkv",
			wantOK: false,
		},
		{
			name:   "daily date-based name has no SxxExx",
			input:  "Show.2024.06.20.1080p.mkv",
			wantOK: false,
		},
		{
			name:   "anime absolute numbering has no SxxExx",
			input:  "[Group] Show - 123 [1080p].mkv",
			wantOK: false,
		},
		{
			name:   "two distinct SxxExx tokens are ambiguous",
			input:  "Show.S01E01.and.S02E02.mkv",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			season, episode, ok := parseSeasonEpisode(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("parseSeasonEpisode(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if season != tt.wantSeason || episode != tt.wantEpisode {
				t.Fatalf("parseSeasonEpisode(%q) = (S%d, E%d), want (S%d, E%d)",
					tt.input, season, episode, tt.wantSeason, tt.wantEpisode)
			}
		})
	}
}

func TestPathUnderRoot(t *testing.T) {
	cases := []struct {
		filePath, folder string
		want             bool
	}{
		{"/root/media/Movie/x.mkv", "/root/media", true}, // under root
		{"/root/media", "/root/media", true},             // exact root
		{"/root/media/", "/root/media", true},            // trailing sep then nothing
		{"/root/media2/x.mkv", "/root/media", false},     // sibling prefix must NOT match
		{"/root/mediaX", "/root/media", false},           // prefix without boundary
		{"/root/media/x", "/root/media/", true},          // folder with trailing slash
		{`C:\Media\Movie\x.mkv`, `C:\Media`, true},        // windows separator
		{`C:\Media2\x.mkv`, `C:\Media`, false},            // windows sibling prefix
		{"/a/b", "", false},                              // empty folder never matches
		{"/data/x.mkv", "/", true},                       // posix root manages everything
		{"/data/x.mkv", "//", true},                      // pure separators = root
		{`C:\x.mkv`, `\`, true},                          // windows root
	}
	for _, c := range cases {
		if got := pathUnderRoot(c.filePath, c.folder); got != c.want {
			t.Errorf("pathUnderRoot(%q, %q) = %v, want %v", c.filePath, c.folder, got, c.want)
		}
	}
}
