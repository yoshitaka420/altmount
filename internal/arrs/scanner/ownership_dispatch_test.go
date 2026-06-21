package scanner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/javi11/altmount/internal/arrs/clients"
	"github.com/javi11/altmount/internal/arrs/data"
	"github.com/javi11/altmount/internal/arrs/instances"
	"github.com/javi11/altmount/internal/arrs/model"
	"github.com/javi11/altmount/internal/config"
)

// dispatchManager wires a Manager with real instances + clients managers backed
// by a single config-defined instance pointing at the given test server.
func dispatchManager(srv *httptest.Server, instanceType, instanceName string) *Manager {
	enabled := true
	inst := config.ArrsInstanceConfig{Name: instanceName, URL: srv.URL, APIKey: "test", Enabled: &enabled}
	arrs := config.ArrsConfig{}
	switch instanceType {
	case "radarr":
		arrs.RadarrInstances = []config.ArrsInstanceConfig{inst}
	case "sonarr":
		arrs.SonarrInstances = []config.ArrsInstanceConfig{inst}
	case "lidarr":
		arrs.LidarrInstances = []config.ArrsInstanceConfig{inst}
	case "readarr":
		arrs.ReadarrInstances = []config.ArrsInstanceConfig{inst}
	case "whisparr":
		arrs.WhisparrInstances = []config.ArrsInstanceConfig{inst}
	case "sportarr":
		arrs.SportarrInstances = []config.ArrsInstanceConfig{inst}
	}
	cfg := &config.Config{MountPath: "/mnt", Arrs: arrs}
	getter := func() *config.Config { return cfg }

	instMgr := instances.NewManager(getter, nil)
	clientMgr := clients.NewManager(srv.Client())
	return NewManager(getter, instMgr, clientMgr, data.NewManager(), nil)
}

func TestResolveOwnership_RadarrOwned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/movie/42") {
			_, _ = w.Write([]byte(`{"id":42,"title":"M","hasFile":true,"movieFile":{"id":900,"path":"/m.mkv"}}`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "radarr", "radarr1")
	meta := &model.WebhookMetadata{InstanceName: "radarr1", Movie: &model.MovieMetadata{Id: 42}}

	got := mgr.ResolveOwnership(context.Background(), "/m.mkv", "", meta)
	if got.Status != model.OwnershipOwned {
		t.Fatalf("status = %v; want owned", got.Status)
	}
	if got.InstanceType != "radarr" || got.InstanceName != "radarr1" {
		t.Errorf("instance = %s/%s; want radarr/radarr1", got.InstanceType, got.InstanceName)
	}
	if got.SafeToDelete() {
		t.Errorf("SafeToDelete = true for owned; want false")
	}
}

func TestResolveOwnership_RadarrReplaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/movie/42") {
			_, _ = w.Write([]byte(`{"id":42,"title":"M","hasFile":true,"movieFile":{"id":900,"path":"/m-2160p.mkv"}}`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "radarr", "radarr1")
	meta := &model.WebhookMetadata{
		InstanceName: "radarr1",
		Movie:        &model.MovieMetadata{Id: 42},
		MovieFile:    &model.MovieFileMetadata{Id: 111}, // different from current 900
	}

	got := mgr.ResolveOwnership(context.Background(), "/m.mkv", "", meta)
	if got.Status != model.OwnershipReplaced {
		t.Fatalf("status = %v; want replaced", got.Status)
	}
	if got.ReplacementID != 900 {
		t.Errorf("replacementID = %d; want 900", got.ReplacementID)
	}
	if !got.SafeToDelete() {
		t.Errorf("SafeToDelete = false for replaced; want true")
	}
}

func TestResolveOwnership_RadarrUnowned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No metadata movie id -> full-list scan with no match.
		if strings.HasSuffix(r.URL.Path, "/movie") {
			_, _ = w.Write([]byte(`[{"id":1,"title":"Other","hasFile":true,"movieFile":{"id":5,"path":"/other.mkv"}}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "radarr", "radarr1")
	meta := &model.WebhookMetadata{InstanceName: "radarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/gone.mkv", "", meta)
	if got.Status != model.OwnershipUnowned {
		t.Fatalf("status = %v; want unowned", got.Status)
	}
	if !got.SafeToDelete() {
		t.Errorf("SafeToDelete = false for unowned; want true")
	}
}

func TestResolveOwnership_RadarrErrorFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Full-list scan errors out -> hard failure -> Unknown.
		if strings.HasSuffix(r.URL.Path, "/movie") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "radarr", "radarr1")
	meta := &model.WebhookMetadata{InstanceName: "radarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/x.mkv", "", meta)
	if got.Status != model.OwnershipUnknown {
		t.Fatalf("status = %v; want unknown (fail closed)", got.Status)
	}
	if got.SafeToDelete() {
		t.Errorf("SafeToDelete = true on error; want false (fail closed)")
	}
}

func TestResolveOwnership_SonarrOwned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ID-based resolution (no series/episodeFile HTTP), then episodes fetch.
		if strings.HasSuffix(r.URL.Path, "/episode") && r.URL.Query().Get("seriesId") == "12" {
			_, _ = w.Write([]byte(`[{"id":1,"seasonNumber":1,"episodeNumber":5,"hasFile":true,"episodeFileId":555}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "sonarr", "sonarr1")
	meta := &model.WebhookMetadata{
		InstanceName: "sonarr1",
		Series:       &model.SeriesMetadata{Id: 12},
		EpisodeFile:  &model.EpisodeFileMetadata{Id: 555},
	}

	got := mgr.ResolveOwnership(context.Background(), "/tv/Show/Season 01/Show.S01E05.mkv", "", meta)
	if got.Status != model.OwnershipOwned {
		t.Fatalf("status = %v; want owned (file still referenced)", got.Status)
	}
}

func TestResolveOwnership_SonarrReplaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/episode") && r.URL.Query().Get("seriesId") == "12" {
			// File 555 no longer referenced; episode S01E05 now has a different file 999.
			_, _ = w.Write([]byte(`[{"id":1,"seasonNumber":1,"episodeNumber":5,"hasFile":true,"episodeFileId":999}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "sonarr", "sonarr1")
	meta := &model.WebhookMetadata{
		InstanceName: "sonarr1",
		Series:       &model.SeriesMetadata{Id: 12},
		EpisodeFile:  &model.EpisodeFileMetadata{Id: 555},
	}

	got := mgr.ResolveOwnership(context.Background(), "/tv/Show/Season 01/Show.S01E05.mkv", "", meta)
	if got.Status != model.OwnershipReplaced {
		t.Fatalf("status = %v; want replaced", got.Status)
	}
	if got.ReplacementID != 999 {
		t.Errorf("replacementID = %d; want 999", got.ReplacementID)
	}
}

func TestResolveOwnership_SonarrSeriesOwnedNoFileSkipsEpisodeFetch(t *testing.T) {
	// Series matches by path but no episode-file record lines up (episodeFileID == 0).
	// The dispatcher must short-circuit to Owned WITHOUT fetching episodes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/series"):
			_, _ = w.Write([]byte(`[{"id":12,"title":"Breaking Bad","path":"/tv/Breaking Bad"}]`))
		case strings.HasSuffix(r.URL.Path, "/episodeFile") && r.URL.Query().Get("seriesId") == "12":
			// A file record exists but does not match our path/filename.
			_, _ = w.Write([]byte(`[{"id":1,"path":"/tv/Breaking Bad/Season 01/Unrelated.mkv"}]`))
		case strings.HasSuffix(r.URL.Path, "/episode"):
			t.Errorf("episodes must NOT be fetched when episodeFileID == 0: %s", r.URL.String())
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
		}
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "sonarr", "sonarr1")
	meta := &model.WebhookMetadata{InstanceName: "sonarr1"}

	got := mgr.ResolveOwnership(context.Background(),
		"/tv/Breaking Bad/Season 01/Breaking Bad S01E01.mkv", "", meta)
	if got.Status != model.OwnershipOwned {
		t.Fatalf("status = %v; want owned (series owns the area, no file record)", got.Status)
	}
}

func TestResolveOwnership_SonarrUnowned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path-based with no series match.
		if strings.HasSuffix(r.URL.Path, "/series") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "sonarr", "sonarr1")
	meta := &model.WebhookMetadata{InstanceName: "sonarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/tv/Unknown/file.S01E01.mkv", "", meta)
	if got.Status != model.OwnershipUnowned {
		t.Fatalf("status = %v; want unowned", got.Status)
	}
}

func TestResolveOwnership_NonIntrospectableFailsClosed(t *testing.T) {
	// Sportarr uses a native (non-starr) API and is intentionally not introspected,
	// so triage must never act on its files. No HTTP call should be made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to non-introspectable arr: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "sportarr", "sportarr1")
	meta := &model.WebhookMetadata{InstanceName: "sportarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/sports/event.mkv", "", meta)
	if got.Status != model.OwnershipUnknown {
		t.Fatalf("status = %v; want unknown for non-introspectable arr", got.Status)
	}
	if got.SafeToDelete() {
		t.Errorf("SafeToDelete = true; want false (fail closed)")
	}
}

func TestResolveOwnership_NoInstanceFailsClosed(t *testing.T) {
	// No metadata instance and no instances configured -> Unknown.
	cfg := &config.Config{MountPath: "/mnt"}
	getter := func() *config.Config { return cfg }
	mgr := NewManager(getter, instances.NewManager(getter, nil), clients.NewManager(nil), data.NewManager(), nil)

	got := mgr.ResolveOwnership(context.Background(), "/x.mkv", "", nil)
	if got.Status != model.OwnershipUnknown {
		t.Fatalf("status = %v; want unknown", got.Status)
	}
}

func TestResolveOwnership_WhisparrOwned(t *testing.T) {
	// Whisparr uses the Sonarr API; ID-based metadata + an episode referencing the
	// file id resolves to Owned.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/episode") && r.URL.Query().Get("seriesId") == "12" {
			_, _ = w.Write([]byte(`[{"id":1,"seasonNumber":1,"episodeNumber":5,"hasFile":true,"episodeFileId":555}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "whisparr", "whisparr1")
	meta := &model.WebhookMetadata{
		InstanceName: "whisparr1",
		Series:       &model.SeriesMetadata{Id: 12},
		EpisodeFile:  &model.EpisodeFileMetadata{Id: 555},
	}

	got := mgr.ResolveOwnership(context.Background(), "/xxx/Show/Season 01/Show.S01E05.mkv", "", meta)
	if got.Status != model.OwnershipOwned {
		t.Fatalf("status = %v; want owned", got.Status)
	}
	if got.InstanceType != "whisparr" {
		t.Errorf("instanceType = %s; want whisparr", got.InstanceType)
	}
}

func TestResolveOwnership_LidarrOwned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET /api/v1/artist (list all)
		if strings.HasSuffix(r.URL.Path, "/artist") {
			_, _ = w.Write([]byte(`[{"id":3,"path":"/music/Pink Floyd"}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "lidarr", "lidarr1")
	meta := &model.WebhookMetadata{InstanceName: "lidarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/music/Pink Floyd/The Wall/track.flac", "", meta)
	if got.Status != model.OwnershipOwned {
		t.Fatalf("status = %v; want owned (file under artist folder)", got.Status)
	}
	if got.SafeToDelete() {
		t.Errorf("SafeToDelete = true for owned; want false")
	}
}

func TestResolveOwnership_LidarrUnowned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/artist") {
			_, _ = w.Write([]byte(`[{"id":3,"path":"/music/Pink Floyd"}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "lidarr", "lidarr1")
	meta := &model.WebhookMetadata{InstanceName: "lidarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/music/Unknown Artist/x.flac", "", meta)
	if got.Status != model.OwnershipUnowned {
		t.Fatalf("status = %v; want unowned (no artist folder matches)", got.Status)
	}
	if !got.SafeToDelete() {
		t.Errorf("SafeToDelete = false for unowned; want true")
	}
}

func TestResolveOwnership_LidarrErrorFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/artist") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "lidarr", "lidarr1")
	meta := &model.WebhookMetadata{InstanceName: "lidarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/music/x/y.flac", "", meta)
	if got.Status != model.OwnershipUnknown {
		t.Fatalf("status = %v; want unknown (fail closed)", got.Status)
	}
}

func TestResolveOwnership_ReadarrOwned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET /api/v1/author (list all)
		if strings.HasSuffix(r.URL.Path, "/author") {
			_, _ = w.Write([]byte(`[{"id":8,"path":"/books/Brandon Sanderson"}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "readarr", "readarr1")
	meta := &model.WebhookMetadata{InstanceName: "readarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/books/Brandon Sanderson/Mistborn/book.epub", "", meta)
	if got.Status != model.OwnershipOwned {
		t.Fatalf("status = %v; want owned (file under author folder)", got.Status)
	}
}

func TestResolveOwnership_ReadarrUnowned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/author") {
			_, _ = w.Write([]byte(`[{"id":8,"path":"/books/Brandon Sanderson"}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := dispatchManager(srv, "readarr", "readarr1")
	meta := &model.WebhookMetadata{InstanceName: "readarr1"}

	got := mgr.ResolveOwnership(context.Background(), "/books/Unknown Author/x.epub", "", meta)
	if got.Status != model.OwnershipUnowned {
		t.Fatalf("status = %v; want unowned", got.Status)
	}
}
