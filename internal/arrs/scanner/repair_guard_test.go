package scanner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/javi11/altmount/internal/arrs/model"
)

// sonarrRepairRecorder captures the destructive calls the Sonarr repair path can
// make so tests can assert whether a delete/search actually fired.
type sonarrRepairRecorder struct {
	deletedFileIDs []string
	searchCalled   bool
}

// newSonarrRepairServer emulates the slice of the Sonarr API that
// triggerSonarrRescanByPath exercises on the season/episode fallback route. The
// episodes list always exposes a single S01E01 whose current episodeFileId is
// curFileID, so the fallback resolves it by SeasonNumber+EpisodeNumber.
func newSonarrRepairServer(t *testing.T, rec *sonarrRepairRecorder, episodeFilePath string, curFileID int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		curID := strconv.FormatInt(curFileID, 10)

		// DELETE /api/v3/episodeFile/{id} — the destructive call under test.
		if r.Method == http.MethodDelete && strings.Contains(path, "/episodeFile/") {
			rec.deletedFileIDs = append(rec.deletedFileIDs, strings.TrimPrefix(path[strings.Index(path, "/episodeFile/"):], "/episodeFile/"))
			_, _ = w.Write([]byte(`{}`))
			return
		}

		switch {
		// GetEpisodeFiles (list) — used by the id-path Smart Repair Guard.
		case strings.Contains(path, "/episodeFile"):
			_, _ = w.Write([]byte(`[{"id":` + curID + `,"seriesId":12,"path":"` + episodeFilePath + `","sceneName":"Show.S01E01.REPACK-GRP"}]`))
		// GetSeriesEpisodesContext.
		case strings.HasSuffix(path, "/episode"):
			_, _ = w.Write([]byte(`[{"id":5001,"seriesId":12,"seasonNumber":1,"episodeNumber":1,"episodeFileId":` + curID + `,"hasFile":true,"title":"Pilot"}]`))
		// Path-based series resolution.
		case strings.HasSuffix(path, "/series"):
			_, _ = w.Write([]byte(`[{"id":12,"title":"Show","path":"/tv/Show"}]`))
		// Queue lookup (empty -> queue-based failure does not match).
		case strings.HasSuffix(path, "/queue"):
			_, _ = w.Write([]byte(`{"page":1,"pageSize":500,"totalRecords":0,"records":[]}`))
		// History lookup for blocklisting (empty -> no-op, non-fatal).
		case strings.HasSuffix(path, "/history"):
			_, _ = w.Write([]byte(`{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`))
		// EpisodeSearch command.
		case strings.HasSuffix(path, "/command"):
			rec.searchCalled = true
			_, _ = w.Write([]byte(`{"id":1,"name":"EpisodeSearch"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

// TestTriggerSonarrRescanByPath_FallbackGuardsHealthyReplacement is the regression
// test for the season/episode-fallback delete: when metadata reports the corrupted
// episode-file id but the episode now points at a DIFFERENT file (a healthy
// upgrade/re-import), the fallback must NOT delete that replacement. It should bail
// with ErrEpisodeAlreadySatisfied and issue no delete/search.
func TestTriggerSonarrRescanByPath_FallbackGuardsHealthyReplacement(t *testing.T) {
	rec := &sonarrRepairRecorder{}
	// Episode's current file id 200 differs from the reported corrupted id 100,
	// and its path differs from the requested path so the id-path Smart Repair
	// Guard does not catch it first — the season/episode fallback is reached.
	srv := newSonarrRepairServer(t, rec, "/tv/Show/Season 01/Show.S01E01.REPACK.mkv", 200)
	defer srv.Close()

	mgr := newResolverManager(nil)
	meta := &model.WebhookMetadata{
		Series:      &model.SeriesMetadata{Id: 12},
		EpisodeFile: &model.EpisodeFileMetadata{Id: 100},
	}

	err := mgr.triggerSonarrRescanByPath(
		context.Background(), newSonarrClient(srv),
		"/tv/Show/Season 01/Show.S01E01.mkv", "", "sonarr1", meta)

	if !errors.Is(err, model.ErrEpisodeAlreadySatisfied) {
		t.Fatalf("err = %v; want ErrEpisodeAlreadySatisfied", err)
	}
	if len(rec.deletedFileIDs) != 0 {
		t.Errorf("episode file(s) deleted = %v; want none (healthy replacement must be preserved)", rec.deletedFileIDs)
	}
	if rec.searchCalled {
		t.Errorf("search command was issued; want none (repair must be skipped)")
	}
}

// TestTriggerSonarrRescanByPath_FallbackDeletesWhenNoReportedFileID proves the guard
// is targeted, not a blanket disable: with no reported episode-file id to compare
// against (a pure path/scene fallback), the season/episode fallback still deletes the
// stale record and re-searches.
func TestTriggerSonarrRescanByPath_FallbackDeletesWhenNoReportedFileID(t *testing.T) {
	rec := &sonarrRepairRecorder{}
	// The current file's path does not match the requested path (a Sonarr rename),
	// so path/filename resolution fails and the season/episode fallback runs. With
	// no metadata file id, the guard does not apply and the delete proceeds.
	srv := newSonarrRepairServer(t, rec, "/tv/Show/Season 01/Show.S01E01.RENAMED.mkv", 200)
	defer srv.Close()

	mgr := newResolverManager(nil)

	err := mgr.triggerSonarrRescanByPath(
		context.Background(), newSonarrClient(srv),
		"/tv/Show/Season 01/Show.S01E01.mkv", "", "sonarr1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.deletedFileIDs) != 1 || rec.deletedFileIDs[0] != "200" {
		t.Errorf("deleted file ids = %v; want [200]", rec.deletedFileIDs)
	}
	if !rec.searchCalled {
		t.Errorf("search command was not issued; want a re-search after the fallback delete")
	}
}
