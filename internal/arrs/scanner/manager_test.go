package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/javi11/altmount/internal/arrs/clients"
	"github.com/javi11/altmount/internal/arrs/data"
	"github.com/javi11/altmount/internal/arrs/instances"
	"github.com/javi11/altmount/internal/config"
	"golift.io/starr"
	"golift.io/starr/sonarr"
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

// TestTriggerSonarrRescanByPath_FallbackDoesNotDeleteHealthyFile exercises the
// season/episode fallback delete path directly. It is reached only when the
// metadata file ID and the file path/filename fail to match the file we were
// asked to repair, but the parsed SxxExx still matches an episode Sonarr owns.
// Because a SxxExx-only match cannot prove Sonarr's current file is the
// stale/corrupt record (rather than a healthy rename / re-import / upgrade at a
// new path), the fallback must blocklist + re-search but MUST NOT delete the
// current episode file. This guards against silently regressing back to deleting
// a possibly-healthy replacement.
func TestTriggerSonarrRescanByPath_FallbackDoesNotDeleteHealthyFile(t *testing.T) {
	const (
		episodeID     = int64(101)
		currentFileID = int64(201)
	)

	tests := []struct {
		name        string
		repairPath  string // path AltMount asks to repair (stale/corrupt)
		currentPath string // path Sonarr currently owns for S01E01 (healthy)
	}{
		{
			name:        "renamed file",
			repairPath:  "/tv/Show/Season 01/Show.S01E01.OLDNAME.mkv",
			currentPath: "/tv/Show/Season 01/Show.S01E01.RENAMED.mkv",
		},
		{
			name:        "re-imported file",
			repairPath:  "/tv/Show/Season 01/Show.S01E01.WEB.mkv",
			currentPath: "/tv/Show/Season 01/Show.S01E01.WEB-DL.REPACK.mkv",
		},
		{
			name:        "healthy upgrade at new path",
			repairPath:  "/tv/Show/Season 01/Show.S01E01.720p.mkv",
			currentPath: "/tv/Show/Season 01/Show.S01E01.2160p.PROPER.mkv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu             sync.Mutex
				deletedFileIDs []string
				searchCmdName  string
				searchedEpIDs  []int64
			)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				p := r.URL.Path
				switch {
				case r.Method == http.MethodDelete && strings.Contains(p, "/episodeFile/"):
					mu.Lock()
					deletedFileIDs = append(deletedFileIDs, p[strings.LastIndex(p, "/")+1:])
					mu.Unlock()
					_, _ = w.Write([]byte(`{}`))
				case strings.HasSuffix(p, "/series"):
					_, _ = w.Write([]byte(`[{"id":1,"title":"Show","path":"/tv/Show"}]`))
				case strings.HasSuffix(p, "/episodeFile"):
					_, _ = w.Write([]byte(fmt.Sprintf(`[{"id":%d,"seriesId":1,"path":%q}]`, currentFileID, tt.currentPath)))
				case strings.HasSuffix(p, "/episode"):
					_, _ = w.Write([]byte(fmt.Sprintf(`[{"id":%d,"seriesId":1,"seasonNumber":1,"episodeNumber":1,"hasFile":true,"episodeFileId":%d}]`, episodeID, currentFileID)))
				case strings.HasSuffix(p, "/queue"):
					_, _ = w.Write([]byte(`{"page":1,"pageSize":500,"sortKey":"timeleft","sortDirection":"ascending","totalRecords":0,"records":[]}`))
				case strings.HasSuffix(p, "/history"):
					_, _ = w.Write([]byte(`{"page":1,"pageSize":100,"sortKey":"date","sortDirection":"descending","totalRecords":0,"records":[]}`))
				case r.Method == http.MethodPost && strings.HasSuffix(p, "/command"):
					var cmd struct {
						Name       string  `json:"name"`
						EpisodeIDs []int64 `json:"episodeIds"`
					}
					_ = json.NewDecoder(r.Body).Decode(&cmd)
					mu.Lock()
					searchCmdName = cmd.Name
					searchedEpIDs = cmd.EpisodeIDs
					mu.Unlock()
					_, _ = w.Write([]byte(`{"id":555}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			defer srv.Close()

			cfg := &config.Config{MountPath: "/tv"}
			mgr := NewManager(func() *config.Config { return cfg }, nil, nil, data.NewManager(), nil)
			client := sonarr.New(&starr.Config{URL: srv.URL, APIKey: "test", Client: srv.Client()})

			err := mgr.triggerSonarrRescanByPath(context.Background(), client, tt.repairPath, "", "sonarr1", nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()

			// The guard: the healthy file Sonarr currently owns must NOT be deleted.
			if len(deletedFileIDs) != 0 {
				t.Errorf("episode file(s) deleted = %v; want none (healthy replacement must be preserved)", deletedFileIDs)
			}
			// Recovery must still happen via a targeted re-search of the matched episode.
			if searchCmdName != "EpisodeSearch" {
				t.Errorf("search command = %q; want EpisodeSearch", searchCmdName)
			}
			if len(searchedEpIDs) != 1 || searchedEpIDs[0] != episodeID {
				t.Errorf("searched episode IDs = %v; want [%d]", searchedEpIDs, episodeID)
			}
		})
	}
}
