package scanner

import (
	"context"
	"testing"

	"github.com/javi11/altmount/internal/arrs/clients"
	"github.com/javi11/altmount/internal/arrs/instances"
	"github.com/javi11/altmount/internal/config"
)

func TestParseSonarrSeasonEpisode(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantSeason  int
		wantEpisode int
		wantOK      bool
	}{
		{
			name:        "standard single episode",
			path:        "/tv/Show/Season 01/Show.S01E05.1080p.WEB-DL.mkv",
			wantSeason:  1,
			wantEpisode: 5,
			wantOK:      true,
		},
		{
			name:        "standard single episode strm",
			path:        "/tv/Show/Season 02/Show.S02E10.2160p.WEB.mkv.strm",
			wantSeason:  2,
			wantEpisode: 10,
			wantOK:      true,
		},
		{
			name:        "lowercase sxxexx",
			path:        "show.s03e07.hdtv.mkv",
			wantSeason:  3,
			wantEpisode: 7,
			wantOK:      true,
		},
		{
			name:        "high season and episode numbers",
			path:        "Show.S2023E145.mkv",
			wantSeason:  2023,
			wantEpisode: 145,
			wantOK:      true,
		},
		{
			name:   "multi-episode back to back left unmatched",
			path:   "Show.S01E01E02.1080p.mkv",
			wantOK: false,
		},
		{
			name:   "multi-episode dashed E range left unmatched",
			path:   "Show.S01E01-E02.1080p.mkv",
			wantOK: false,
		},
		{
			name:   "multi-episode bare range left unmatched",
			path:   "Show.S01E01-02.mkv",
			wantOK: false,
		},
		{
			name:        "dashed resolution suffix is single episode",
			path:        "Show.S01E01-1080p.WEB.mkv",
			wantSeason:  1,
			wantEpisode: 1,
			wantOK:      true,
		},
		{
			name:   "daily date-based left unmatched",
			path:   "Show.2023.05.18.1080p.mkv",
			wantOK: false,
		},
		{
			name:   "anime absolute numbering left unmatched",
			path:   "Show - 145 [1080p].mkv",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			season, episode, ok := parseSonarrSeasonEpisode(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v; want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if season != tt.wantSeason {
				t.Errorf("season = %d; want %d", season, tt.wantSeason)
			}
			if episode != tt.wantEpisode {
				t.Errorf("episode = %d; want %d", episode, tt.wantEpisode)
			}
		})
	}
}

func TestFindInstanceForFilePath_CategoryMatch(t *testing.T) {
	enabled := true

	tests := []struct {
		name        string
		completeDir string
		filePath    string
		radarrCat   string
		sonarrCat   string
		wantType    string
		wantName    string
		wantErr     bool
	}{
		{
			name:        "completeDir set to /complete, matches radarr by category moviesHQ",
			completeDir: "/complete",
			filePath:    "/mnt/disk1/cloud/downloads/complete/moviesHQ/The.Devil.Wears.Prada.2006.2160p/The.Devil...mkv",
			radarrCat:   "moviesHQ",
			sonarrCat:   "tvHQ",
			wantType:    "radarr",
			wantName:    "radarr_instance",
		},
		{
			name:        "completeDir set to /complete, matches sonarr by category tvHQ",
			completeDir: "/complete",
			filePath:    "/mnt/disk1/cloud/downloads/complete/tvHQ/Abbott.Elementary/Abbott...mkv",
			radarrCat:   "moviesHQ",
			sonarrCat:   "tvHQ",
			wantType:    "sonarr",
			wantName:    "sonarr_instance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				MountPath: "/mnt/disk1/cloud/downloads",
				SABnzbd: config.SABnzbdConfig{
					CompleteDir: tt.completeDir,
				},
				Arrs: config.ArrsConfig{
					RadarrInstances: []config.ArrsInstanceConfig{
						{
							Name:     "radarr_instance",
							Category: tt.radarrCat,
							Enabled:  &enabled,
							URL:      "http://localhost:7878",
						},
					},
					SonarrInstances: []config.ArrsInstanceConfig{
						{
							Name:     "sonarr_instance",
							Category: tt.sonarrCat,
							Enabled:  &enabled,
							URL:      "http://localhost:8989",
						},
					},
				},
			}

			configGetter := func() *config.Config {
				return cfg
			}

			instMgr := instances.NewManager(configGetter, nil)
			clientMgr := clients.NewManager(nil)
			mgr := NewManager(configGetter, instMgr, clientMgr, nil, nil)

			gotType, gotName, err := mgr.findInstanceForFilePath(context.Background(), tt.filePath, "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("unexpected error result: %v", err)
			}

			if gotType != tt.wantType {
				t.Errorf("gotType = %q; want %q", gotType, tt.wantType)
			}
			if gotName != tt.wantName {
				t.Errorf("gotName = %q; want %q", gotName, tt.wantName)
			}
		})
	}
}

// TestRadarrFileMatchesRepairTarget covers the path-anchored confirmation that
// gates the destructive Radarr repair (DeleteMovieFilesContext). The critical
// guarantee is that a filename-only collision (a same-named healthy file at a
// different/renamed directory) is NOT confirmed as the corrupt target.
func TestRadarrFileMatchesRepairTarget(t *testing.T) {
	tests := []struct {
		name         string
		moviePath    string
		filePath     string
		relativePath string
		want         bool
	}{
		{
			name:      "exact path match",
			moviePath: "/lib/Movie (2010)/Movie.2010.mkv",
			filePath:  "/lib/Movie (2010)/Movie.2010.mkv",
			want:      true,
		},
		{
			name:      "strm-stripped full path match",
			moviePath: "/lib/Movie (2010)/Movie.2010.mkv",
			filePath:  "/lib/Movie (2010)/Movie.2010.strm",
			want:      true,
		},
		{
			name:         "relative suffix match",
			moviePath:    "/data/media/movies/Movie (2010)/Movie.2010.mkv",
			filePath:     "/altmount/whatever/Movie.2010.mkv",
			relativePath: "movies/Movie (2010)/Movie.2010.mkv",
			want:         true,
		},
		{
			name:         "relative suffix match with .strm relative path",
			moviePath:    "/data/movies/Movie (2010)/Movie.2010.mkv",
			filePath:     "/altmount/whatever/Movie.2010.mkv.strm",
			relativePath: "movies/Movie (2010)/Movie.2010.strm",
			want:         true,
		},
		{
			name:         "different movie, different path and name",
			moviePath:    "/lib/Other (2011)/Other.2011.mkv",
			filePath:     "/lib/Movie (2010)/Movie.2010.mkv.strm",
			relativePath: "Movie (2010)/Movie.2010.mkv.strm",
			want:         false,
		},
		{
			// Regression guard: a same-named healthy file at a renamed folder
			// (symlink library, real media rescan path) must NOT be confirmed,
			// because filename-only matching is intentionally excluded here.
			name:         "same filename, different (renamed) directory",
			moviePath:    "/data/Movie Renamed (2010)/Movie.2010.mkv",
			filePath:     "/library/Movie (2010)/Movie.2010.mkv",
			relativePath: "Movie (2010)/Movie.2010.mkv",
			want:         false,
		},
		{
			name:      "no relative path, no anchored match",
			moviePath: "/lib/A/Movie.2010.mkv",
			filePath:  "/other/B/Movie.2010.mkv",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := radarrFileMatchesRepairTarget(tt.moviePath, tt.filePath, tt.relativePath)
			if got != tt.want {
				t.Errorf("radarrFileMatchesRepairTarget(%q, %q, %q) = %v; want %v",
					tt.moviePath, tt.filePath, tt.relativePath, got, tt.want)
			}
		})
	}
}
