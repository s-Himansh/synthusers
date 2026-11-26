// Package storepg implements the synthusers.Store interface on PostgreSQL
// (pgx). It is the store to use for durable, multi-instance deployments:
// every shard of the fleet reads targets and writes issues into the same
// database, so the dashboard shows one combined picture.
package storepg

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"synthusers"
)

//go:embed schema.sql
var schema string

// Store is a PostgreSQL-backed synthusers.Store.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to the database, applies the schema and returns a Store.
func New(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() error { s.pool.Close(); return nil }

// ---- targets -------------------------------------------------------------

func (s *Store) UpsertTarget(ctx context.Context, t synthusers.Target) error {
	spec, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO targets (id, name, enabled, spec, created_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (id) DO UPDATE
		 SET name = EXCLUDED.name, enabled = EXCLUDED.enabled, spec = EXCLUDED.spec`,
		t.ID, t.Name, t.Enabled, spec)
	return err
}

func (s *Store) DeleteTarget(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM targets WHERE id = $1`, id)
	return err
}

func (s *Store) ListTargets(ctx context.Context) ([]synthusers.Target, error) {
	rows, err := s.pool.Query(ctx, `SELECT spec FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []synthusers.Target
	for rows.Next() {
		var spec []byte
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		var t synthusers.Target
		if err := json.Unmarshal(spec, &t); err != nil {
			return nil, fmt.Errorf("decode target spec: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- runs ----------------------------------------------------------------

func (s *Store) SaveRun(ctx context.Context, r *synthusers.Run) error {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO runs (target_id, mode, started_at, duration_ms, pages, requests,
		                   new_issues, shard, worker, persona, error)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id`,
		r.TargetID, r.Mode, r.StartedAt, r.Duration.Std().Milliseconds(),
		r.Pages, r.Requests, r.NewIssues, r.Shard, r.Worker, r.Persona, r.Error)
	return row.Scan(&r.ID)
}

func (s *Store) ListRuns(ctx context.Context, targetID string, limit int) ([]synthusers.Run, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	if targetID == "" {
		rows, err = s.pool.Query(ctx,
			`SELECT id, target_id, mode, started_at, duration_ms, pages, requests,
			        new_issues, shard, worker, persona, error
			 FROM runs ORDER BY started_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT id, target_id, mode, started_at, duration_ms, pages, requests,
			        new_issues, shard, worker, persona, error
			 FROM runs WHERE target_id = $1 ORDER BY started_at DESC LIMIT $2`, targetID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []synthusers.Run
	for rows.Next() {
		var r synthusers.Run
		var durMS int64
		if err := rows.Scan(&r.ID, &r.TargetID, &r.Mode, &r.StartedAt, &durMS,
			&r.Pages, &r.Requests, &r.NewIssues, &r.Shard, &r.Worker, &r.Persona, &r.Error); err != nil {
			return nil, err
		}
		r.Duration = synthusers.Duration(time.Duration(durMS) * time.Millisecond)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- issues --------------------------------------------------------------

func (s *Store) ReportFinding(ctx context.Context, targetID string, f synthusers.Finding) (bool, error) {
	if f.URLKey == "" {
		f.URLKey = f.URL
	}
	fp := synthusers.FingerprintOf(targetID, f)
	now := time.Now()
	// xmax = 0 is true only for freshly inserted rows.
	var inserted bool
	err := s.pool.QueryRow(ctx,
		`INSERT INTO issues (fingerprint, target_id, type, severity, url, url_key,
		                     summary, detail, status, first_seen, last_seen, occurrences)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,1)
		 ON CONFLICT (fingerprint) DO UPDATE SET
			summary = EXCLUDED.summary,
			detail = EXCLUDED.detail,
			severity = EXCLUDED.severity,
			url = EXCLUDED.url,
			last_seen = EXCLUDED.last_seen,
			occurrences = issues.occurrences + 1,
			status = 'open',
			resolved_at = NULL
		 RETURNING (xmax = 0)`,
		fp, targetID, f.Type, string(f.Severity), f.URL, f.URLKey,
		f.Summary, f.Detail, synthusers.StatusOpen, now).Scan(&inserted)
	if err != nil {
		return false, err
	}
	return inserted, nil
}

func (s *Store) ResolveIssues(ctx context.Context, fingerprints []string) (int, error) {
	if len(fingerprints) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE issues SET status = 'resolved', resolved_at = now()
		 WHERE status = 'open' AND fingerprint = ANY($1)`,
		fingerprints)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) ListIssues(ctx context.Context, f synthusers.IssueFilter) ([]synthusers.Issue, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	where := []string{"TRUE"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Status != "" {
		add(`status = $%d`, f.Status)
	}
	if f.Severity != "" {
		add(`severity = $%d`, f.Severity)
	}
	if f.TargetID != "" {
		add(`target_id = $%d`, f.TargetID)
	}
	if f.Type != "" {
		add(`type = $%d`, f.Type)
	}
	args = append(args, f.Limit)
	q := `SELECT fingerprint, target_id, type, severity, url, url_key, summary, detail,
	             status, first_seen, last_seen, occurrences, resolved_at
	      FROM issues WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY (status = 'open') DESC,
		          CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
		          last_seen DESC
	      LIMIT $` + fmt.Sprint(len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []synthusers.Issue
	for rows.Next() {
		var iss synthusers.Issue
		var sev string
		if err := rows.Scan(&iss.Fingerprint, &iss.TargetID, &iss.Type, &sev,
			&iss.URL, &iss.URLKey, &iss.Summary, &iss.Detail, &iss.Status,
			&iss.FirstSeen, &iss.LastSeen, &iss.Occurrences, &iss.ResolvedAt); err != nil {
			return nil, err
		}
		iss.Severity = synthusers.Severity(sev)
		out = append(out, iss)
	}
	return out, rows.Err()
}

func (s *Store) Stats(ctx context.Context) (synthusers.Stats, error) {
	st := synthusers.Stats{BySeverity: map[string]int{}, ByType: map[string]int{}}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM targets`).Scan(&st.Targets); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return st, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT severity, type, count(*) FROM issues WHERE status = 'open' GROUP BY severity, type`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var sev, typ string
		var n int
		if err := rows.Scan(&sev, &typ, &n); err != nil {
			return st, err
		}
		st.OpenIssues += n
		st.BySeverity[sev] += n
		st.ByType[typ] += n
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	return st, s.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(pages),0) FROM runs WHERE started_at > now() - interval '24 hours'`).
		Scan(&st.RunsLast24h, &st.PagesLast24h)
}
