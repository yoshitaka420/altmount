package streamcheck

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const maxStreamBlocklistRemoteDownloadBytes = 256 * 1024 * 1024
const streamBlocklistSourceStatusOK = "Ok"

type StreamBlocklistRemoteSourceService struct {
	store *StreamBlocklistStore
	http  *http.Client
	once  sync.Once
}

func NewStreamBlocklistRemoteSourceService(store *StreamBlocklistStore) *StreamBlocklistRemoteSourceService {
	return &StreamBlocklistRemoteSourceService{
		store: store,
		http: &http.Client{
			Timeout: 90 * time.Second,
			Transport: &http.Transport{
				DisableCompression: true,
			},
		},
	}
}

func (s *StreamBlocklistRemoteSourceService) Start(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	s.once.Do(func() {
		go s.loop(ctx)
	})
}

func (s *StreamBlocklistRemoteSourceService) loop(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := s.RefreshDue(ctx); err != nil {
				slog.DebugContext(ctx, "stream blocklist remote-source loop failed", "error", err)
			}
			timer.Reset(15 * time.Minute)
		}
	}
}

func (s *StreamBlocklistRemoteSourceService) RefreshDue(ctx context.Context) error {
	sources, err := s.store.GetSources(ctx)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, source := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if source.Kind != "remote" || source.URL == "" {
			continue
		}
		if now-source.LastChecked < int64(source.RefreshHours)*3600 {
			continue
		}
		_, _ = s.Refresh(ctx, source)
	}
	return nil
}

func (s *StreamBlocklistRemoteSourceService) Refresh(ctx context.Context, source StreamBlocklistSourceInfo) (string, error) {
	now := time.Now().Unix()
	if source.URL == "" {
		_ = s.store.TouchChecked(ctx, source.ID, now, "error: no url")
		return "error: no url", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		msg := "error: " + err.Error()
		_ = s.store.TouchChecked(ctx, source.ID, now, msg)
		return msg, err
	}
	if etag := s.store.GetSourceEtag(ctx, source.ID); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		msg := "error: " + err.Error()
		_ = s.store.TouchChecked(ctx, source.ID, now, msg)
		return msg, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		_ = s.store.TouchChecked(ctx, source.ID, now, streamBlocklistSourceStatusOK)
		return streamBlocklistSourceStatusOK, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("HTTP %d", resp.StatusCode)
		msg := "error: " + err.Error()
		_ = s.store.TouchChecked(ctx, source.ID, now, msg)
		return msg, err
	}
	body, err := readLimited(resp.Body, maxStreamBlocklistRemoteDownloadBytes)
	if err != nil {
		msg := "error: " + err.Error()
		_ = s.store.TouchChecked(ctx, source.ID, now, msg)
		return msg, err
	}
	reader, closeFn, err := maybeGzipReader(bytes.NewReader(body))
	if err != nil {
		msg := "error: " + err.Error()
		_ = s.store.TouchChecked(ctx, source.ID, now, msg)
		return msg, err
	}
	defer closeFn()
	if _, err := s.store.ReplaceSource(ctx, source.ID, reader); err != nil {
		msg := "error: " + err.Error()
		_ = s.store.TouchChecked(ctx, source.ID, now, msg)
		return msg, err
	}
	etag := ""
	if resp.Header.Get("ETag") != "" {
		etag = resp.Header.Get("ETag")
	}
	msg := streamBlocklistSourceStatusOK
	if err := s.store.SetSourceStatus(ctx, source.ID, etag, now, now, msg); err != nil {
		return msg, err
	}
	return msg, nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	lr := &io.LimitedReader{R: r, N: limit + 1}
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download exceeds the size limit")
	}
	return data, nil
}

func maybeGzipReader(r io.Reader) (io.Reader, func(), error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, func() {}, err
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, func() {}, err
		}
		return gz, func() { _ = gz.Close() }, nil
	}
	return bytes.NewReader(data), func() {}, nil
}
