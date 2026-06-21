package scanner

import (
	"context"
	"testing"

	"github.com/javi11/altmount/internal/arrs/clients"
	"github.com/javi11/altmount/internal/arrs/instances"
	"github.com/javi11/altmount/internal/config"
)

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
