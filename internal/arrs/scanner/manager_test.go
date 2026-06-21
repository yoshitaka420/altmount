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
			// grabHistoryID is the "grabbed" history record that blocklisting must
			// mark as failed (POST /history/failed/{id}) so the re-search can't
			// simply re-grab the same dead release.
			const grabHistoryID = "777"

			var (
				mu                    sync.Mutex
				deletedFileIDs        []string
				blocklistedHistoryIDs []string
				searchCmdName         string
				searchedEpIDs         []int64
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
				case r.Method == http.MethodPost && strings.Contains(p, "/history/failed/"):
					mu.Lock()
					blocklistedHistoryIDs = append(blocklistedHistoryIDs, p[strings.LastIndex(p, "/")+1:])
					mu.Unlock()
					_, _ = w.Write([]byte(`{}`))
				case strings.HasSuffix(p, "/history"):
					// Import event links the current file to a download; grab event is
					// what blocklisting marks as failed.
					_, _ = w.Write([]byte(fmt.Sprintf(`{"page":1,"pageSize":100,"sortKey":"date","sortDirection":"descending","totalRecords":2,"records":[`+
						`{"id":1,"eventType":"downloadFolderImported","downloadId":"abc","data":{"fileId":"%d"}},`+
						`{"id":%s,"eventType":"grabbed","downloadId":"abc"}]}`, currentFileID, grabHistoryID)))
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
			// The dead release must still be blocklisted so the re-search below
			// won't simply re-grab it.
			if len(blocklistedHistoryIDs) != 1 || blocklistedHistoryIDs[0] != grabHistoryID {
				t.Errorf("blocklisted history IDs = %v; want [%s]", blocklistedHistoryIDs, grabHistoryID)
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

// TestTriggerSonarrRescanByPath_RelativePathFallbackRejectsSibling verifies the
// secondary series match (folder name against the relative path) is anchored at a
// path-component boundary. The repaired file belongs to "Showcase", but "Show" is
// a substring of "Showcase" and is listed first, so the old strings.Contains match
// would wrongly select "Show". The primary full-path match is forced to miss (the
// mount prefix differs from the *arr library root) so resolution falls through to
// the relative-path folder-name branch.
func TestTriggerSonarrRescanByPath_RelativePathFallbackRejectsSibling(t *testing.T) {
	var (
		mu                  sync.Mutex
		episodeFileSeriesID string
		episodeSeriesID     string
		deletedFileIDs      []string
		searchedEpIDs       []int64
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
			// "Show" is a substring of "Showcase" and is listed first, so a naive
			// substring match would grab "Show" for a Showcase file. Library roots
			// use a /data/tv prefix that the repair path below does NOT share.
			_, _ = w.Write([]byte(`[{"id":1,"title":"Show","path":"/data/tv/Show"},{"id":2,"title":"Showcase","path":"/data/tv/Showcase"}]`))
		case strings.HasSuffix(p, "/episodeFile"):
			mu.Lock()
			episodeFileSeriesID = r.URL.Query().Get("seriesId")
			mu.Unlock()
			// Filename differs from the repair path so the file scan misses and the
			// season/episode fallback (blocklist-only) handles recovery.
			_, _ = w.Write([]byte(`[{"id":402,"seriesId":2,"path":"/data/tv/Showcase/Season 01/Showcase.S01E01.NEW.mkv"}]`))
		case strings.HasSuffix(p, "/episode"):
			mu.Lock()
			episodeSeriesID = r.URL.Query().Get("seriesId")
			mu.Unlock()
			_, _ = w.Write([]byte(`[{"id":222,"seriesId":2,"seasonNumber":1,"episodeNumber":1,"hasFile":true,"episodeFileId":402}]`))
		case strings.HasSuffix(p, "/queue"):
			_, _ = w.Write([]byte(`{"page":1,"pageSize":500,"sortKey":"timeleft","sortDirection":"ascending","totalRecords":0,"records":[]}`))
		case strings.HasSuffix(p, "/history"):
			_, _ = w.Write([]byte(`{"page":1,"pageSize":100,"sortKey":"date","sortDirection":"descending","totalRecords":0,"records":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/command"):
			var cmd struct {
				EpisodeIDs []int64 `json:"episodeIds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&cmd)
			mu.Lock()
			searchedEpIDs = cmd.EpisodeIDs
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":555}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{MountPath: "/mnt/lib"}
	mgr := NewManager(func() *config.Config { return cfg }, nil, nil, data.NewManager(), nil)
	client := sonarr.New(&starr.Config{URL: srv.URL, APIKey: "test", Client: srv.Client()})

	// Repair path's mount prefix (/mnt/lib) differs from the library root
	// (/data/tv), so the primary full-path match misses for both series and
	// resolution falls through to the relative-path folder-name branch.
	err := mgr.triggerSonarrRescanByPath(context.Background(), client,
		"/mnt/lib/Showcase/Season 01/Showcase.S01E01.OLD.mkv",
		"Showcase/Season 01/Showcase.S01E01.OLD.mkv", "sonarr1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// The correct series ("Showcase", id 2) must be selected — never the substring
	// sibling "Show" (id 1).
	if episodeFileSeriesID != "2" {
		t.Errorf("episodeFile seriesId = %q; want \"2\" (Showcase, not Show)", episodeFileSeriesID)
	}
	if episodeSeriesID != "2" {
		t.Errorf("episode seriesId = %q; want \"2\" (Showcase, not Show)", episodeSeriesID)
	}
	if len(searchedEpIDs) != 1 || searchedEpIDs[0] != 222 {
		t.Errorf("searched episode IDs = %v; want [222]", searchedEpIDs)
	}
	if len(deletedFileIDs) != 0 {
		t.Errorf("episode file(s) deleted = %v; want none", deletedFileIDs)
	}
}

// TestSonarrHasFile_RejectsSiblingFolder verifies the instance-routing folder-name
// check is anchored at a path-component boundary: a file under "Showcase" must not
// be reported as owned by an instance that only has the "Show" series, since "Show"
// is a substring of "Showcase".
func TestSonarrHasFile_RejectsSiblingFolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/series") {
			_, _ = w.Write([]byte(`[{"id":1,"title":"Show","path":"/data/tv/Show"}]`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := &config.Config{MountPath: "/mnt/lib"}
	mgr := NewManager(func() *config.Config { return cfg }, nil, nil, data.NewManager(), nil)
	client := sonarr.New(&starr.Config{URL: srv.URL, APIKey: "test", Client: srv.Client()})

	// Sibling: file belongs to "Showcase" but only "Show" exists -> must NOT match.
	if mgr.sonarrHasFile(context.Background(), client, "sonarr1", "Showcase/Season 01/Showcase.S01E01.mkv") {
		t.Error("sonarrHasFile matched a Showcase file against the Show series (sibling substring); want no match")
	}
	// Genuine: file actually belongs to "Show" -> must match.
	if !mgr.sonarrHasFile(context.Background(), client, "sonarr1", "Show/Season 01/Show.S01E01.mkv") {
		t.Error("sonarrHasFile did not match a genuine Show file; want match")
	}
}
