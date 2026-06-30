package streamcheck

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/javi11/altmount/internal/config"
	_ "github.com/mattn/go-sqlite3"
)

const (
	LocalSourceID    = "local"
	TrustFull        = "full"
	TrustCorroborate = "corroborate"
	TrustObserve     = "observe"
)

var wardenFPPattern = regexp.MustCompile(`^wd1:[0-9a-f]{32}$`)

type WardenStore struct {
	db           *sql.DB
	configGetter config.ConfigGetter
}

type WardenRecord struct {
	Fp        string   `json:"fp"`
	Backbones []string `json:"bk,omitempty"`
	DeadAt    int64    `json:"deadAt"`
	Count     int      `json:"n"`
}

type WardenSourceInfo struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	URL          string `json:"url,omitempty"`
	Enabled      bool   `json:"enabled"`
	Trust        string `json:"trust"`
	RefreshHours int    `json:"refreshHours"`
	LastChecked  int64  `json:"lastChecked"`
	LastUpdated  int64  `json:"lastUpdated"`
	Status       string `json:"status,omitempty"`
	Count        int    `json:"count"`
}

type RemoteSourceSpec struct {
	URL          string `json:"url"`
	Name         string `json:"name,omitempty"`
	Trust        string `json:"trust,omitempty"`
	RefreshHours int    `json:"refreshHours,omitempty"`
}

func NewWardenStore(configGetter config.ConfigGetter) (*WardenStore, error) {
	dbPath := defaultWardenDBPath(configGetter)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &WardenStore{db: db, configGetter: configGetter}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	store.tryMigrateLegacyJSON(dbPath)
	return store, nil
}

func (s *WardenStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func IsValidWardenFingerprint(fp string) bool {
	return wardenFPPattern.MatchString(fp)
}

func defaultWardenDBPath(configGetter config.ConfigGetter) string {
	if configGetter != nil {
		if cfg := configGetter(); cfg != nil {
			if p := strings.TrimSpace(cfg.GetStreamCheckWardenDBPath()); p != "" {
				return p
			}
			if cfg.Database.Path != "" {
				return filepath.Join(filepath.Dir(cfg.Database.Path), "warden.db")
			}
		}
	}
	return "warden.db"
}

func (s *WardenStore) initialize(ctx context.Context) error {
	stmts := []string{
		"PRAGMA busy_timeout=5000;",
		"PRAGMA journal_mode=WAL;",
		"CREATE TABLE IF NOT EXISTS warden_sources (id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL, url TEXT, enabled INTEGER NOT NULL DEFAULT 1, trust TEXT NOT NULL DEFAULT 'full', refresh_hours INTEGER NOT NULL DEFAULT 24, last_checked INTEGER NOT NULL DEFAULT 0, last_updated INTEGER NOT NULL DEFAULT 0, etag TEXT, status TEXT, sort INTEGER NOT NULL DEFAULT 0);",
		"CREATE TABLE IF NOT EXISTS warden_entries (source_id TEXT NOT NULL DEFAULT 'local', fp TEXT NOT NULL, dead_at INTEGER NOT NULL, n INTEGER NOT NULL, backbones TEXT NOT NULL DEFAULT '', PRIMARY KEY (source_id, fp));",
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := s.migrateEntriesSchema(ctx); err != nil {
		return err
	}
	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS ix_warden_fp ON warden_entries(fp);",
		"CREATE INDEX IF NOT EXISTS ix_warden_dead_at ON warden_entries(dead_at);",
		"INSERT OR IGNORE INTO warden_sources (id, kind, name, url, enabled, trust, refresh_hours) VALUES ('local', 'local', 'My list', NULL, 1, 'full', 24);",
		"UPDATE warden_sources SET trust = 'full' WHERE trust <> 'full';",
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *WardenStore) migrateEntriesSchema(ctx context.Context) error {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='warden_entries'").Scan(&exists)
	if err != nil || exists == 0 {
		return err
	}
	var hasSourceID int
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('warden_entries') WHERE name='source_id'").Scan(&hasSourceID)
	if err != nil || hasSourceID > 0 {
		return err
	}
	stmts := []string{
		"ALTER TABLE warden_entries RENAME TO warden_entries_legacy;",
		"CREATE TABLE warden_entries (source_id TEXT NOT NULL DEFAULT 'local', fp TEXT NOT NULL, dead_at INTEGER NOT NULL, n INTEGER NOT NULL, backbones TEXT NOT NULL DEFAULT '', PRIMARY KEY (source_id, fp));",
		"INSERT OR IGNORE INTO warden_entries (source_id, fp, dead_at, n, backbones) SELECT 'local', fp, dead_at, n, backbones FROM warden_entries_legacy;",
		"DROP TABLE warden_entries_legacy;",
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *WardenStore) Count(ctx context.Context) int {
	return s.scalarInt(ctx, "SELECT COUNT(*) FROM warden_entries")
}

func (s *WardenStore) LocalCount(ctx context.Context) int {
	return s.scalarInt(ctx, "SELECT COUNT(*) FROM warden_entries WHERE source_id = 'local'")
}

func (s *WardenStore) EffectiveCount(ctx context.Context) int {
	q := s.quorum()
	var out int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM (SELECT e.fp FROM warden_entries e JOIN warden_sources src ON src.id = e.source_id WHERE src.enabled = 1 AND src.trust IN ('full','corroborate') GROUP BY e.fp HAVING MAX(src.trust = 'full') = 1 OR COUNT(*) >= ?)",
		q,
	).Scan(&out)
	if err != nil {
		slog.DebugContext(ctx, "Warden effective-count failed", "error", err)
		return 0
	}
	return out
}

func (s *WardenStore) MarkDead(ctx context.Context, fp string) error {
	if !IsValidWardenFingerprint(fp) {
		return nil
	}
	now := time.Now().Unix()
	existing := ""
	err := s.db.QueryRowContext(ctx, "SELECT backbones FROM warden_entries WHERE source_id = 'local' AND fp = ?", fp).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO warden_entries (source_id, fp, dead_at, n, backbones) VALUES ('local', ?, ?, 1, ?) ON CONFLICT(source_id, fp) DO UPDATE SET dead_at = excluded.dead_at, n = warden_entries.n + 1, backbones = excluded.backbones",
		fp,
		now,
		mergeBackbones(existing, s.currentBackbones()),
	)
	return err
}

func (s *WardenStore) IsDeadAnywhere(ctx context.Context, fp string) bool {
	if !IsValidWardenFingerprint(fp) {
		return false
	}
	scope := s.backboneScope()
	mine := map[string]struct{}{}
	if scope {
		for _, b := range s.currentBackbones() {
			rb := WardenRootDomain(b)
			if rb != "unknown" {
				mine[rb] = struct{}{}
			}
		}
		if len(mine) == 0 {
			scope = false
		}
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT src.trust, e.backbones, e.source_id FROM warden_entries e JOIN warden_sources src ON src.id = e.source_id WHERE e.fp = ? AND src.enabled = 1 AND src.trust IN ('full','corroborate')",
		fp,
	)
	if err != nil {
		slog.DebugContext(ctx, "Warden lookup failed", "error", err)
		return false
	}
	defer rows.Close()
	agree := 0
	quorum := s.quorum()
	for rows.Next() {
		var trust, backbones, sourceID string
		if err := rows.Scan(&trust, &backbones, &sourceID); err != nil {
			continue
		}
		if scope && sourceID != LocalSourceID && !backboneInScope(backbones, mine) {
			continue
		}
		if trust == TrustFull {
			return true
		}
		agree++
		if agree >= quorum {
			return true
		}
	}
	return false
}

func (s *WardenStore) GetSources(ctx context.Context) ([]WardenSourceInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT src.id, src.kind, src.name, COALESCE(src.url, ''), src.enabled, src.trust, src.refresh_hours, src.last_checked, src.last_updated, COALESCE(src.status, ''), (SELECT COUNT(*) FROM warden_entries e WHERE e.source_id = src.id) FROM warden_sources src ORDER BY CASE WHEN src.id = 'local' THEN 0 ELSE 1 END, src.sort, src.name",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []WardenSourceInfo
	for rows.Next() {
		var source WardenSourceInfo
		var enabled int
		if err := rows.Scan(&source.ID, &source.Kind, &source.Name, &source.URL, &enabled, &source.Trust, &source.RefreshHours, &source.LastChecked, &source.LastUpdated, &source.Status, &source.Count); err != nil {
			return nil, err
		}
		source.Enabled = enabled != 0
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func (s *WardenStore) AddSource(ctx context.Context, kind, name, sourceURL, trust string, refreshHours int) (string, error) {
	id := "src_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if strings.TrimSpace(name) == "" {
		name = "Untitled"
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO warden_sources (id, kind, name, url, enabled, trust, refresh_hours, last_checked, last_updated, status, sort) VALUES (?, ?, ?, NULLIF(?, ''), 1, ?, ?, 0, 0, NULL, (SELECT COALESCE(MAX(sort),0)+1 FROM warden_sources))",
		id,
		kind,
		strings.TrimSpace(name),
		strings.TrimSpace(sourceURL),
		normalizeTrust(trust),
		clampRefreshHours(refreshHours),
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *WardenStore) ImportRemoteSources(ctx context.Context, specs []RemoteSourceSpec) (int, int, error) {
	if len(specs) == 0 {
		return 0, 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	existing := map[string]struct{}{}
	rows, err := tx.QueryContext(ctx, "SELECT url FROM warden_sources WHERE url IS NOT NULL")
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			existing[strings.ToLower(u)] = struct{}{}
		}
	}
	rows.Close()
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO warden_sources (id, kind, name, url, enabled, trust, refresh_hours, last_checked, last_updated, status, sort) VALUES (?, 'remote', ?, ?, 1, ?, ?, 0, 0, NULL, (SELECT COALESCE(MAX(sort),0)+1 FROM warden_sources))")
	if err != nil {
		return 0, 0, err
	}
	defer stmt.Close()
	added := 0
	skipped := 0
	for _, spec := range specs {
		u := strings.TrimSpace(spec.URL)
		key := strings.ToLower(u)
		if u == "" {
			skipped++
			continue
		}
		if _, ok := existing[key]; ok {
			skipped++
			continue
		}
		existing[key] = struct{}{}
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = "Untitled"
		}
		id := "src_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		if _, err := stmt.ExecContext(ctx, id, name, u, normalizeTrust(spec.Trust), clampRefreshHours(spec.RefreshHours)); err != nil {
			return 0, 0, err
		}
		added++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return added, skipped, nil
}

func (s *WardenStore) UpdateSource(ctx context.Context, id string, enabled *bool, trust *string, refreshHours *int, name *string) error {
	if id == "" || id == LocalSourceID {
		return nil
	}
	sets := []string{}
	args := []any{}
	if enabled != nil {
		sets = append(sets, "enabled = ?")
		if *enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if trust != nil {
		sets = append(sets, "trust = ?")
		args = append(args, normalizeTrust(*trust))
	}
	if refreshHours != nil {
		sets = append(sets, "refresh_hours = ?")
		args = append(args, clampRefreshHours(*refreshHours))
	}
	if name != nil {
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*name))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := s.db.ExecContext(ctx, "UPDATE warden_sources SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

func (s *WardenStore) ClearSource(ctx context.Context, id string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM warden_entries WHERE source_id = ?", id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *WardenStore) RemoveSource(ctx context.Context, id string) (bool, error) {
	if id == "" || id == LocalSourceID {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM warden_entries WHERE source_id = ?", id); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM warden_sources WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

func (s *WardenStore) Clear(ctx context.Context) (int64, error) {
	return s.ClearSource(ctx, LocalSourceID)
}

func (s *WardenStore) SetSourceStatus(ctx context.Context, id, etag string, lastChecked, lastUpdated int64, status string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE warden_sources SET etag = NULLIF(?, ''), last_checked = ?, last_updated = ?, status = NULLIF(?, '') WHERE id = ?", etag, lastChecked, lastUpdated, status, id)
	return err
}

func (s *WardenStore) TouchChecked(ctx context.Context, id string, when int64, status string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE warden_sources SET last_checked = ?, status = NULLIF(?, '') WHERE id = ?", when, status, id)
	return err
}

func (s *WardenStore) GetSourceEtag(ctx context.Context, id string) string {
	var etag sql.NullString
	_ = s.db.QueryRowContext(ctx, "SELECT etag FROM warden_sources WHERE id = ?", id).Scan(&etag)
	return etag.String
}

func (s *WardenStore) MergeIntoLocal(ctx context.Context, input io.Reader) (int, error) {
	return s.loadInto(ctx, input, LocalSourceID, false)
}

func (s *WardenStore) ImportAsNewSource(ctx context.Context, input io.Reader, name, trust string) (string, int, error) {
	id, err := s.AddSource(ctx, "imported", name, "", trust, 24)
	if err != nil {
		return "", 0, err
	}
	count, err := s.loadInto(ctx, input, id, true)
	if err != nil {
		return id, 0, err
	}
	now := time.Now().Unix()
	_ = s.SetSourceStatus(ctx, id, "", now, now, fmt.Sprintf("imported %d", count))
	return id, count, nil
}

func (s *WardenStore) ReplaceSource(ctx context.Context, id string, input io.Reader) (int, error) {
	return s.loadInto(ctx, input, id, true)
}

func (s *WardenStore) loadInto(ctx context.Context, input io.Reader, sourceID string, replace bool) (int, error) {
	capEntries := s.maxSourceEntries()
	now := time.Now().Unix()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if replace {
		if _, err := tx.ExecContext(ctx, "DELETE FROM warden_entries WHERE source_id = ?", sourceID); err != nil {
			return 0, err
		}
	}
	sel, upsert, err := prepareWardenImport(ctx, tx)
	if err != nil {
		return 0, err
	}
	defer sel.Close()
	defer upsert.Close()
	processed := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, `{"warden"`) {
			continue
		}
		var rec WardenRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if !IsValidWardenFingerprint(rec.Fp) {
			continue
		}
		if processed >= capEntries {
			return 0, fmt.Errorf("source exceeds the %d-fingerprint limit", capEntries)
		}
		existing := ""
		if err := sel.QueryRowContext(ctx, sourceID, rec.Fp).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		deadAt := rec.DeadAt
		if deadAt < 0 {
			deadAt = 0
		}
		if deadAt > now+86400 {
			deadAt = now + 86400
		}
		n := rec.Count
		if n <= 0 {
			n = 1
		}
		if n > 1_000_000_000 {
			n = 1_000_000_000
		}
		if _, err := upsert.ExecContext(ctx, sourceID, rec.Fp, deadAt, n, mergeBackbones(existing, rec.Backbones)); err != nil {
			return 0, err
		}
		processed++
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if replace && processed == 0 {
		return 0, errors.New("no valid fingerprints found")
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return processed, nil
}

func prepareWardenImport(ctx context.Context, tx *sql.Tx) (*sql.Stmt, *sql.Stmt, error) {
	sel, err := tx.PrepareContext(ctx, "SELECT backbones FROM warden_entries WHERE source_id = ? AND fp = ?")
	if err != nil {
		return nil, nil, err
	}
	upsert, err := tx.PrepareContext(ctx, "INSERT INTO warden_entries (source_id, fp, dead_at, n, backbones) VALUES (?, ?, ?, ?, ?) ON CONFLICT(source_id, fp) DO UPDATE SET dead_at = MAX(warden_entries.dead_at, excluded.dead_at), n = MIN(warden_entries.n + excluded.n, 1000000000), backbones = excluded.backbones")
	if err != nil {
		sel.Close()
		return nil, nil, err
	}
	return sel, upsert, nil
}

func (s *WardenStore) ExportTo(ctx context.Context, output io.Writer, sourceIDs []string, dedup bool) error {
	if len(sourceIDs) == 0 {
		sourceIDs = []string{LocalSourceID}
	}
	bw := bufio.NewWriterSize(output, 1<<16)
	if _, err := fmt.Fprintf(bw, "{\"warden\":1,\"updated\":%d}\n", time.Now().Unix()); err != nil {
		return err
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(sourceIDs)), ",")
	args := make([]any, len(sourceIDs))
	for i, id := range sourceIDs {
		args[i] = id
	}
	query := "SELECT fp, dead_at, n, backbones FROM warden_entries WHERE source_id IN (" + placeholders + ")"
	if dedup {
		query = "SELECT fp, MAX(dead_at), SUM(n), group_concat(backbones, ',') FROM warden_entries WHERE source_id IN (" + placeholders + ") GROUP BY fp"
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	enc := json.NewEncoder(bw)
	for rows.Next() {
		var rec WardenRecord
		var backbones string
		if err := rows.Scan(&rec.Fp, &rec.DeadAt, &rec.Count, &backbones); err != nil {
			return err
		}
		rec.Backbones = splitBackbones(backbones)
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return bw.Flush()
}

func (s *WardenStore) currentBackbones() []string {
	if s.configGetter == nil {
		return []string{"unknown"}
	}
	cfg := s.configGetter()
	if cfg == nil {
		return []string{"unknown"}
	}
	set := map[string]struct{}{}
	for _, p := range cfg.Providers {
		b := WardenBackbone(p.Host)
		if b != "unknown" {
			set[b] = struct{}{}
		}
	}
	if len(set) == 0 {
		return []string{"unknown"}
	}
	out := make([]string, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	return out
}

func (s *WardenStore) quorum() int {
	if s.configGetter == nil {
		return 2
	}
	if cfg := s.configGetter(); cfg != nil {
		return max(1, cfg.GetStreamCheckWardenQuorum())
	}
	return 2
}

func (s *WardenStore) maxSourceEntries() int {
	if s.configGetter == nil {
		return 2_000_000
	}
	if cfg := s.configGetter(); cfg != nil {
		return cfg.GetStreamCheckWardenMaxSourceEntries()
	}
	return 2_000_000
}

func (s *WardenStore) backboneScope() bool {
	if s.configGetter == nil {
		return true
	}
	if cfg := s.configGetter(); cfg != nil {
		return cfg.GetStreamCheckWardenBackboneScopeEnabled()
	}
	return true
}

func (s *WardenStore) scalarInt(ctx context.Context, query string) int {
	var out int
	if err := s.db.QueryRowContext(ctx, query).Scan(&out); err != nil {
		slog.DebugContext(ctx, "Warden scalar failed", "error", err)
		return 0
	}
	return out
}

func normalizeTrust(trust string) string {
	return TrustFull
}

func clampRefreshHours(hours int) int {
	if hours < 1 {
		return 24
	}
	if hours > 24*30 {
		return 24 * 30
	}
	return hours
}

func mergeBackbones(existing string, add []string) string {
	set := map[string]struct{}{}
	for _, b := range splitBackbones(existing) {
		if b != "" {
			set[b] = struct{}{}
		}
	}
	for _, b := range add {
		b = strings.TrimSpace(b)
		if b != "" {
			set[b] = struct{}{}
		}
	}
	if len(set) == 0 {
		return "unknown"
	}
	out := make([]string, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	return strings.Join(out, ",")
}

func splitBackbones(csv string) []string {
	if csv == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func backboneInScope(entry string, mine map[string]struct{}) bool {
	known := false
	for _, b := range splitBackbones(entry) {
		rb := WardenRootDomain(b)
		if rb == "unknown" {
			continue
		}
		known = true
		if _, ok := mine[rb]; ok {
			return true
		}
	}
	return !known
}

func (s *WardenStore) tryMigrateLegacyJSON(dbPath string) {
	ctx := context.Background()
	if s.Count(ctx) > 0 {
		return
	}
	jsonPath := filepath.Join(filepath.Dir(dbPath), "warden.json")
	f, err := os.Open(jsonPath)
	if err != nil {
		return
	}
	defer f.Close()
	var model struct {
		Entries []WardenRecord `json:"entries"`
	}
	if err := json.NewDecoder(f).Decode(&model); err != nil || len(model.Entries) == 0 {
		return
	}
	pr, pw := io.Pipe()
	go func() {
		enc := json.NewEncoder(pw)
		for _, rec := range model.Entries {
			if err := enc.Encode(rec); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		_ = pw.Close()
	}()
	if _, err := s.MergeIntoLocal(ctx, pr); err == nil {
		_ = os.Rename(jsonPath, jsonPath+".migrated")
	}
}
