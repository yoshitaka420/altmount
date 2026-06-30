package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/javi11/altmount/internal/streamcheck"
)

const (
	maxWardenUploadBytes       = 256 * 1024 * 1024
	maxWardenSourcesUploadByte = 5 * 1024 * 1024
	maxWardenSourceItems       = 1000
)

type wardenSourcesResponse struct {
	Quorum         int                            `json:"quorum"`
	LocalCount     int                            `json:"localCount"`
	EffectiveCount int                            `json:"effectiveCount"`
	TotalRows      int                            `json:"totalRows"`
	Sources        []streamcheck.WardenSourceInfo `json:"sources"`
}

type wardenImportResponse struct {
	Added    int    `json:"added"`
	Total    int    `json:"total"`
	Cleared  int64  `json:"cleared"`
	SourceID string `json:"sourceId,omitempty"`
}

type wardenSourceMutateResponse struct {
	SourceID string `json:"sourceId,omitempty"`
	Message  string `json:"message,omitempty"`
	Removed  int64  `json:"removed,omitempty"`
}

type wardenSourcesImportResponse struct {
	Added   int `json:"added"`
	Skipped int `json:"skipped"`
	Invalid int `json:"invalid"`
}

func (s *Server) requireWarden(c *fiber.Ctx) (*streamcheck.WardenStore, bool) {
	if s.wardenStore == nil {
		_ = RespondServiceUnavailable(c, "Stream Blocklist store not available", "")
		return nil, false
	}
	return s.wardenStore, true
}

func (s *Server) handleGetWarden(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	return RespondSuccess(c, fiber.Map{"count": store.Count(c.Context())})
}

func (s *Server) handleWardenSources(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	sources, err := store.GetSources(c.Context())
	if err != nil {
		return RespondInternalError(c, "Failed to list Stream Blocklist sources", err.Error())
	}
	cfg := s.configManager.GetConfig()
	quorum := 2
	if cfg != nil {
		quorum = cfg.GetStreamCheckWardenQuorum()
	}
	return RespondSuccess(c, wardenSourcesResponse{
		Quorum:         quorum,
		LocalCount:     store.LocalCount(c.Context()),
		EffectiveCount: store.EffectiveCount(c.Context()),
		TotalRows:      store.Count(c.Context()),
		Sources:        sources,
	})
}

func (s *Server) handleWardenImport(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	ctx := c.Context()
	if c.FormValue("action") == "clear" {
		removed, err := store.Clear(ctx)
		if err != nil {
			return RespondInternalError(c, "Failed to clear Stream Blocklist", err.Error())
		}
		return RespondSuccess(c, wardenImportResponse{Added: 0, Total: store.Count(ctx), Cleared: removed})
	}
	file, err := firstFormFile(c)
	if err != nil {
		return RespondBadRequest(c, "No file was uploaded", err.Error())
	}
	data, err := readUploadedFile(file, maxWardenUploadBytes)
	if err != nil {
		return RespondBadRequest(c, "Failed to read Stream Blocklist import", err.Error())
	}
	body, err := decodeMaybeGzip(data, maxWardenUploadBytes)
	if err != nil {
		return RespondBadRequest(c, "Failed to decode Stream Blocklist import", err.Error())
	}
	if c.FormValue("target") == "separate" {
		name := strings.TrimSpace(c.FormValue("name"))
		if name == "" {
			name = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(file.Filename), ".gz"), ".ndjson")
		}
		if name == "" {
			name = "Imported list"
		}
		sourceID, count, err := store.ImportAsNewSource(ctx, bytes.NewReader(body), name, streamcheck.TrustFull)
		if err != nil {
			return RespondBadRequest(c, "Failed to import Stream Blocklist source", err.Error())
		}
		return RespondSuccess(c, wardenImportResponse{Added: count, Total: store.Count(ctx), SourceID: sourceID})
	}
	before := store.LocalCount(ctx)
	if _, err := store.MergeIntoLocal(ctx, bytes.NewReader(body)); err != nil {
		return RespondBadRequest(c, "Failed to import Stream Blocklist", err.Error())
	}
	after := store.LocalCount(ctx)
	return RespondSuccess(c, wardenImportResponse{Added: max(0, after-before), Total: store.Count(ctx)})
}

func (s *Server) handleWardenExport(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	sourceIDs := []string{streamcheck.LocalSourceID}
	if c.Query("scope") == "merged" {
		if requested := strings.TrimSpace(c.Query("sources")); requested != "" {
			sourceIDs = splitCSV(requested)
		} else {
			sources, err := store.GetSources(c.Context())
			if err != nil {
				return RespondInternalError(c, "Failed to list Stream Blocklist sources", err.Error())
			}
			sourceIDs = sourceIDs[:0]
			for _, source := range sources {
				sourceIDs = append(sourceIDs, source.ID)
			}
		}
	}
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if err := store.ExportTo(c.Context(), gz, sourceIDs, c.Query("dedup") != "0"); err != nil {
		_ = gz.Close()
		return RespondInternalError(c, "Failed to export Stream Blocklist", err.Error())
	}
	if err := gz.Close(); err != nil {
		return RespondInternalError(c, "Failed to finalize Stream Blocklist export", err.Error())
	}
	c.Set("Content-Type", "application/gzip")
	c.Set("Content-Disposition", `attachment; filename="stream-blocklist.ndjson.gz"`)
	return c.Send(out.Bytes())
}

func (s *Server) handleWardenSourceAdd(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	rawURL := strings.TrimSpace(c.FormValue("url"))
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return RespondBadRequest(c, "Enter a valid http(s) URL", "")
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if name == "" {
		name = parsed.Host
	}
	refreshHours := parseIntDefault(c.FormValue("refreshHours"), 24)
	sourceID, err := store.AddSource(c.Context(), "remote", name, rawURL, streamcheck.TrustFull, refreshHours)
	if err != nil {
		return RespondInternalError(c, "Failed to add Stream Blocklist source", err.Error())
	}
	message := ""
	if s.wardenRemote != nil {
		sources, _ := store.GetSources(c.Context())
		for _, source := range sources {
			if source.ID == sourceID {
				message, _ = s.wardenRemote.Refresh(c.Context(), source)
				break
			}
		}
	}
	return RespondSuccess(c, wardenSourceMutateResponse{SourceID: sourceID, Message: message})
}

func (s *Server) handleWardenSourceUpdate(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	id := strings.TrimSpace(c.FormValue("id"))
	if id == "" {
		return RespondBadRequest(c, "Missing source id", "")
	}
	var enabled *bool
	if hasFormKey(c, "enabled") {
		v := c.FormValue("enabled") == "true" || c.FormValue("enabled") == "1"
		enabled = &v
	}
	var refreshHours *int
	if hasFormKey(c, "refreshHours") {
		v := parseIntDefault(c.FormValue("refreshHours"), 24)
		refreshHours = &v
	}
	var name *string
	if hasFormKey(c, "name") {
		v := c.FormValue("name")
		name = &v
	}
	if err := store.UpdateSource(c.Context(), id, enabled, nil, refreshHours, name); err != nil {
		return RespondInternalError(c, "Failed to update Stream Blocklist source", err.Error())
	}
	return RespondSuccess(c, wardenSourceMutateResponse{SourceID: id})
}

func (s *Server) handleWardenSourceRemove(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	id := strings.TrimSpace(c.FormValue("id"))
	if id == "" {
		return RespondBadRequest(c, "Missing source id", "")
	}
	if c.FormValue("action") == "clear" {
		removed, err := store.ClearSource(c.Context(), id)
		if err != nil {
			return RespondInternalError(c, "Failed to clear Stream Blocklist source", err.Error())
		}
		return RespondSuccess(c, wardenSourceMutateResponse{SourceID: id, Removed: removed})
	}
	removed, err := store.RemoveSource(c.Context(), id)
	if err != nil {
		return RespondInternalError(c, "Failed to remove Stream Blocklist source", err.Error())
	}
	var count int64
	if removed {
		count = 1
	}
	return RespondSuccess(c, wardenSourceMutateResponse{SourceID: id, Removed: count})
}

func (s *Server) handleWardenSourceRefresh(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	if s.wardenRemote == nil {
		return RespondServiceUnavailable(c, "Stream Blocklist remote source service not available", "")
	}
	id := strings.TrimSpace(c.FormValue("id"))
	sources, err := store.GetSources(c.Context())
	if err != nil {
		return RespondInternalError(c, "Failed to list Stream Blocklist sources", err.Error())
	}
	for _, source := range sources {
		if source.ID == id {
			message, err := s.wardenRemote.Refresh(c.Context(), source)
			if err != nil {
				return RespondBadRequest(c, "Failed to refresh Stream Blocklist source", err.Error())
			}
			return RespondSuccess(c, wardenSourceMutateResponse{SourceID: id, Message: message})
		}
	}
	return RespondBadRequest(c, "Unknown source", "")
}

func (s *Server) handleWardenSourcesImport(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	defaultTrust := streamcheck.TrustFull
	defaultRefresh := parseIntDefault(c.FormValue("refreshHours"), 24)
	content := c.FormValue("text")
	if file, err := firstFormFile(c); err == nil {
		if file.Size > maxWardenSourcesUploadByte {
			return RespondBadRequest(c, "File is too large", "")
		}
		data, err := readUploadedFile(file, maxWardenSourcesUploadByte)
		if err != nil {
			return RespondBadRequest(c, "Failed to read Stream Blocklist sources import", err.Error())
		}
		content = string(data)
	}
	if strings.TrimSpace(content) == "" {
		return RespondBadRequest(c, "Paste some entries or choose a file", "")
	}
	if len(content) > maxWardenSourcesUploadByte {
		return RespondBadRequest(c, "Input is too large", "")
	}
	specs, invalid := parseRemoteSourceSpecs(content, defaultTrust, defaultRefresh)
	if len(specs) == 0 && invalid == 0 {
		return RespondBadRequest(c, "Nothing found", "")
	}
	added, skipped, err := store.ImportRemoteSources(c.Context(), specs)
	if err != nil {
		return RespondInternalError(c, "Failed to import Stream Blocklist sources", err.Error())
	}
	if added > 0 && s.wardenRemote != nil {
		go func() {
			if err := s.wardenRemote.RefreshDue(context.Background()); err != nil {
				_ = err
			}
		}()
	}
	return RespondSuccess(c, wardenSourcesImportResponse{Added: added, Skipped: skipped, Invalid: invalid})
}

func (s *Server) handleWardenSourcesExport(c *fiber.Ctx) error {
	store, ok := s.requireWarden(c)
	if !ok {
		return nil
	}
	sources, err := store.GetSources(c.Context())
	if err != nil {
		return RespondInternalError(c, "Failed to list Stream Blocklist sources", err.Error())
	}
	type bundleItem struct {
		URL          string `json:"url"`
		Name         string `json:"name,omitempty"`
		Trust        string `json:"trust,omitempty"`
		RefreshHours int    `json:"refreshHours"`
	}
	bundle := struct {
		Version int          `json:"version"`
		Items   []bundleItem `json:"items"`
	}{Version: 1}
	for _, source := range sources {
		if source.Kind == "remote" && source.URL != "" {
			bundle.Items = append(bundle.Items, bundleItem{
				URL:          source.URL,
				Name:         source.Name,
				Trust:        source.Trust,
				RefreshHours: source.RefreshHours,
			})
		}
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return RespondInternalError(c, "Failed to export Stream Blocklist sources", err.Error())
	}
	c.Set("Content-Type", "application/json")
	c.Set("Content-Disposition", `attachment; filename="bundle.json"`)
	return c.Send(data)
}

func firstFormFile(c *fiber.Ctx) (*multipart.FileHeader, error) {
	if file, err := c.FormFile("file"); err == nil {
		return file, nil
	}
	form, err := c.MultipartForm()
	if err != nil {
		return nil, err
	}
	for _, files := range form.File {
		if len(files) > 0 {
			return files[0], nil
		}
	}
	return nil, fmt.Errorf("no file")
}

func readUploadedFile(file *multipart.FileHeader, limit int64) ([]byte, error) {
	f, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lr := &io.LimitedReader{R: f, N: limit + 1}
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds the size limit")
	}
	return data, nil
}

func decodeMaybeGzip(data []byte, limit int64) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	lr := &io.LimitedReader{R: gz, N: limit + 1}
	out, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("decompressed file exceeds the size limit")
	}
	return out, nil
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseIntDefault(raw string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return n
	}
	return fallback
}

func hasFormKey(c *fiber.Ctx, key string) bool {
	if c.Request().PostArgs().Has(key) {
		return true
	}
	form, err := c.MultipartForm()
	if err != nil || form == nil {
		return false
	}
	_, ok := form.Value[key]
	return ok
}

func parseRemoteSourceSpecs(content, defaultTrust string, defaultRefresh int) ([]streamcheck.RemoteSourceSpec, int) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, 0
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if specs, invalid, ok := parseRemoteSourceSpecsJSON(trimmed, defaultTrust, defaultRefresh); ok {
			return specs, invalid
		}
	}
	var specs []streamcheck.RemoteSourceSpec
	invalid := 0
	for _, raw := range strings.Split(content, "\n") {
		if len(specs) >= maxWardenSourceItems {
			break
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if spec, ok := remoteSourceSpec(line, "", defaultTrust, defaultRefresh); ok {
			specs = append(specs, spec)
		} else {
			invalid++
		}
	}
	return specs, invalid
}

func parseRemoteSourceSpecsJSON(content, defaultTrust string, defaultRefresh int) ([]streamcheck.RemoteSourceSpec, int, bool) {
	var root any
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		return nil, 0, false
	}
	if obj, ok := root.(map[string]any); ok {
		if items, ok := obj["items"]; ok {
			root = items
		}
	}
	items, ok := root.([]any)
	if !ok {
		return nil, 0, false
	}
	var specs []streamcheck.RemoteSourceSpec
	invalid := 0
	for _, item := range items {
		if len(specs) >= maxWardenSourceItems {
			break
		}
		switch v := item.(type) {
		case string:
			if spec, ok := remoteSourceSpec(v, "", defaultTrust, defaultRefresh); ok {
				specs = append(specs, spec)
			} else {
				invalid++
			}
		case map[string]any:
			u, _ := v["url"].(string)
			name, _ := v["name"].(string)
			trust, _ := v["trust"].(string)
			refresh := defaultRefresh
			if n, ok := v["refreshHours"].(float64); ok {
				refresh = int(n)
			}
			if strings.TrimSpace(trust) == "" {
				trust = defaultTrust
			}
			if spec, ok := remoteSourceSpec(u, name, trust, refresh); ok {
				specs = append(specs, spec)
			} else {
				invalid++
			}
		default:
			invalid++
		}
	}
	return specs, invalid, true
}

func remoteSourceSpec(rawURL, name, trust string, refreshHours int) (streamcheck.RemoteSourceSpec, bool) {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return streamcheck.RemoteSourceSpec{}, false
	}
	return streamcheck.RemoteSourceSpec{
		URL:          rawURL,
		Name:         strings.TrimSpace(name),
		Trust:        streamcheck.TrustFull,
		RefreshHours: refreshHours,
	}, true
}
