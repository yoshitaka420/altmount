package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Sportarr is a thin native HTTP client for Sportarr.
//
// Unlike Sonarr/Radarr/Lidarr/Readarr/Whisparr, Sportarr is NOT driveable through
// the golift.io/starr library: its /api/v3 surface is a narrow Sonarr-compat shim
// where only system/status and indexer are real, while /api/v3/queue and
// /api/v3/command fall through to the SPA. Sportarr's real, JSON-returning API
// lives at the unversioned /api/* paths (queue, history, pending-imports), so we
// talk to those directly here.
//
// The main management value AltMount adds for Sportarr is queue cleanup; Sportarr
// already self-imports finished downloads on its own poll interval.
type Sportarr struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// SportarrStatusMessage mirrors the Servarr status-message shape used by the
// queue-cleanup allowlist matching.
type SportarrStatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

// SportarrQueueItem maps the fields of a Sportarr native queue record that the
// queue-cleanup worker needs. Field names follow the Servarr convention; unknown
// fields are ignored by the JSON decoder.
type SportarrQueueItem struct {
	ID                    int64                   `json:"id"`
	Title                 string                  `json:"title"`
	Status                string                  `json:"status"`
	Protocol              string                  `json:"protocol"`
	DownloadClient        string                  `json:"downloadClient"`
	OutputPath            string                  `json:"outputPath"`
	TrackedDownloadStatus string                  `json:"trackedDownloadStatus"`
	TrackedDownloadState  string                  `json:"trackedDownloadState"`
	StatusMessages        []SportarrStatusMessage `json:"statusMessages"`
}

// NewSportarr builds a native Sportarr client. The shared *http.Client carries
// the manager's proxy/timeout configuration.
func NewSportarr(baseURL, apiKey string, hc *http.Client) *Sportarr {
	return &Sportarr{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		hc:      hc,
	}
}

func (s *Sportarr) newRequest(ctx context.Context, method, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", s.apiKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// newJSONRequest builds a request carrying a JSON-encoded body (for POST/PUT).
func (s *Sportarr) newJSONRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", s.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// Health verifies connectivity using the one /api/v3 endpoint Sportarr implements
// for real (system/status).
func (s *Sportarr) Health(ctx context.Context) error {
	req, err := s.newRequest(ctx, http.MethodGet, "/api/v3/system/status")
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to Sportarr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Sportarr health check returned status %d", resp.StatusCode)
	}
	return nil
}

// GetQueue returns the current Sportarr queue. Sportarr's native /api/queue may
// return either a bare JSON array or a paged object with a "records" array, so we
// tolerate both shapes.
func (s *Sportarr) GetQueue(ctx context.Context) ([]SportarrQueueItem, error) {
	req, err := s.newRequest(ctx, http.MethodGet, "/api/queue")
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get Sportarr queue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Sportarr queue request returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read Sportarr queue response: %w", err)
	}

	// Try the bare-array shape first.
	var items []SportarrQueueItem
	if err := json.Unmarshal(body, &items); err == nil {
		return items, nil
	}

	// Fall back to the paged-object shape.
	var paged struct {
		Records []SportarrQueueItem `json:"records"`
	}
	if err := json.Unmarshal(body, &paged); err != nil {
		return nil, fmt.Errorf("failed to decode Sportarr queue response: %w", err)
	}
	return paged.Records, nil
}

// DeleteQueueItem removes a queue item, also instructing Sportarr to remove the
// download from the (AltMount) download client. It does not blocklist.
func (s *Sportarr) DeleteQueueItem(ctx context.Context, id int64) error {
	path := fmt.Sprintf("/api/queue/%d?removeFromClient=true&blocklist=false", id)
	req, err := s.newRequest(ctx, http.MethodDelete, path)
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("failed to delete Sportarr queue item %d: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("404: queue item %d not found", id)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Sportarr delete queue item %d returned status %d", id, resp.StatusCode)
	}
	return nil
}

// SportarrNotificationField is a single config field of a Sportarr notification.
type SportarrNotificationField struct {
	Name  string `json:"name"`
	Value any    `json:"value,omitempty"`
}

// SportarrNotification mirrors the Servarr (Sonarr-compatible) notification
// resource. Only the fields AltMount needs to manage its Webhook connection are
// modelled; unknown fields are ignored on decode. Sportarr implements the
// Sonarr-style /api/v3/notification endpoint for real, so AltMount registers its
// webhook here the same way it does for the starr apps.
type SportarrNotification struct {
	ID             int64                       `json:"id,omitempty"`
	Name           string                      `json:"name"`
	Implementation string                      `json:"implementation"`
	ConfigContract string                      `json:"configContract"`
	OnGrab         bool                        `json:"onGrab"`
	OnDownload     bool                        `json:"onDownload"` // OnImport
	OnUpgrade      bool                        `json:"onUpgrade"`
	OnRename       bool                        `json:"onRename"`
	Fields         []SportarrNotificationField `json:"fields"`
}

// GetNotifications lists the configured notifications.
func (s *Sportarr) GetNotifications(ctx context.Context) ([]SportarrNotification, error) {
	req, err := s.newRequest(ctx, http.MethodGet, "/api/v3/notification")
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list Sportarr notifications: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Sportarr notification list returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read Sportarr notification response: %w", err)
	}
	var out []SportarrNotification
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to decode Sportarr notifications (is /api/v3/notification available?): %w", err)
	}
	return out, nil
}

// AddNotification creates a new notification.
func (s *Sportarr) AddNotification(ctx context.Context, notif *SportarrNotification) error {
	req, err := s.newJSONRequest(ctx, http.MethodPost, "/api/v3/notification", notif)
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("failed to add Sportarr notification: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Sportarr add notification returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// UpdateNotification updates an existing notification, identified by notif.ID.
func (s *Sportarr) UpdateNotification(ctx context.Context, notif *SportarrNotification) error {
	path := fmt.Sprintf("/api/v3/notification/%d", notif.ID)
	req, err := s.newJSONRequest(ctx, http.MethodPut, path, notif)
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("failed to update Sportarr notification: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Sportarr update notification returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
