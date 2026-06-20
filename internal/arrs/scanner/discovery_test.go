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
