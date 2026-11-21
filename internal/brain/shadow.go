package brain

import (
	"context"
	"errors"
	"fmt"

	"github.com/alash3al/stash/internal/embedder"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// ShadowMigrator builds a second embedding representation alongside the live
// one, so an embedding-model change can be validated before it is adopted.
//
// The invariant that makes this safe: this type NEVER writes `embedding`,
// `embedding_model`, or `settings.vector_dimension`, and nothing in the recall
// path reads `embedding_shadow`. While a migration runs, production retrieval
// is byte-for-byte what it was before. The only way to adopt the new
// representation is a deliberate, separate read-path swap.
//
// It exists because the retrieval path has no model predicate — RecallFacts
// filters on namespace, vector and limit — so old- and new-model vectors in one
// column are indistinguishable to a query, and mixing them silently degrades
// ranking rather than failing.
type ShadowMigrator struct {
	pool     *pgxpool.Pool
	embedder embedder.Embedder
	model    string
	dims     int
	batch    int
}

// ErrShadowNotConfigured is returned when a shadow operation is requested but
// no shadow model/credential pair is configured. It is a configuration error,
// not a runtime failure — callers should surface it, never retry it.
var ErrShadowNotConfigured = errors.New("shadow embedding is not configured (need STASH_SHADOW_EMBEDDING_MODEL and STASH_DEEPINFRA_EMBEDDING_API_KEY)")

func NewShadowMigrator(pool *pgxpool.Pool, emb embedder.Embedder, model string, dims, batch int) (*ShadowMigrator, error) {
	// Configuration is checked before wiring: "not configured" is a normal
	// operator-facing state and must not be masked by a programming error that
	// bootstrap makes impossible anyway.
	if emb == nil || model == "" {
		return nil, ErrShadowNotConfigured
	}
	if pool == nil {
		return nil, fmt.Errorf("shadow: pool is required")
	}
	if dims <= 0 {
		return nil, fmt.Errorf("shadow: vector dimension must be positive, got %d", dims)
	}
	// pgvector cannot build an HNSW index on a `vector` wider than 2000. Refuse
	// at construction rather than after embedding thousands of rows that then
	// cannot be indexed.
	if dims > 2000 {
		return nil, fmt.Errorf("shadow: vector dimension %d exceeds pgvector's 2000 HNSW limit for the vector type; request a smaller dimension from the provider or use halfvec", dims)
	}
	if batch <= 0 {
		batch = 100
	}
	return &ShadowMigrator{pool: pool, embedder: emb, model: model, dims: dims, batch: batch}, nil
}

// Embedder exposes the shadow embedder so an evaluation can embed queries with
// the SAME model that produced the shadow vectors. Pairing them is mandatory:
// embedding a query with one model and searching a column built by another
// compares incomparable vectors and yields meaningless numbers.
func (s *ShadowMigrator) Embedder() embedder.Embedder { return s.embedder }

// Model returns the shadow model identifier.
func (s *ShadowMigrator) Model() string { return s.model }

// EnsureSchema pins the shadow column dimension and builds its HNSW index.
// Idempotent: safe to call before every wave.
//
// The dimension is applied here rather than in the migration so the target
// stays configuration, not something frozen into a checked-in SQL file.
func (s *ShadowMigrator) EnsureSchema(ctx context.Context) error {
	// All DDL runs on ONE dedicated connection so the parallel-worker settings
	// below actually apply to it. With a pool, each Exec may land on a
	// different backend and a session-level SET would be silently lost.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("shadow: acquire ddl connection: %w", err)
	}
	defer conn.Release()

	// Railway's Postgres has a small /dev/shm, and parallel maintenance workers
	// allocate shared memory segments sized from maintenance_work_mem. Both the
	// ALTER TABLE rewrite and the HNSW build will fail with
	//   "could not resize shared memory segment ... No space left on device"
	//   (SQLSTATE 53100)
	// if they try to parallelise. Serial maintenance is slower and works.
	// Confirmed against production 2026-08-14.
	for _, stmt := range []string{
		"SET max_parallel_maintenance_workers = 0",
		"SET max_parallel_workers_per_gather = 0",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("shadow: %q: %w", stmt, err)
		}
	}

	for _, table := range []string{"episodes", "facts"} {
		var typmod int
		if err := conn.QueryRow(ctx,
			`SELECT atttypmod FROM pg_attribute a
			   JOIN pg_class c ON a.attrelid = c.oid
			  WHERE c.relname = $1 AND a.attname = 'embedding_shadow'`,
			table,
		).Scan(&typmod); err != nil {
			return fmt.Errorf("shadow: inspect %s.embedding_shadow: %w", table, err)
		}

		// For pgvector, atttypmod IS the dimension (-1 while unconstrained).
		// It is NOT the varchar convention of length+4 — assuming that read a
		// correct vector(1024) back as 1020 and refused to proceed.
		if typmod == -1 {
			if _, err := conn.Exec(ctx,
				fmt.Sprintf("ALTER TABLE %s ALTER COLUMN embedding_shadow TYPE vector(%d)", table, s.dims),
			); err != nil {
				return fmt.Errorf("shadow: set %s dimension: %w", table, err)
			}
		} else if typmod != s.dims {
			// A real mismatch means the configured dimension changed after rows
			// were written. Refuse rather than silently reshaping the column.
			return fmt.Errorf(
				"shadow: %s.embedding_shadow is vector(%d) but configured dimension is %d; "+
					"clear the column before changing dimension", table, typmod, s.dims)
		}

		idx := table + "_embedding_shadow_hnsw_idx"
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (embedding_shadow vector_cosine_ops)", idx, table),
		); err != nil {
			return fmt.Errorf("shadow: create %s: %w", idx, err)
		}
	}
	return nil
}

// ShadowProgress reports migration state for one namespace (or the whole
// corpus when NamespaceID is nil).
type ShadowProgress struct {
	Namespace       string `json:"namespace"`
	EpisodesTotal   int    `json:"episodes_total"`
	EpisodesShadow  int    `json:"episodes_shadow"`
	EpisodesPending int    `json:"episodes_pending"`
	FactsTotal      int    `json:"facts_total"`
	FactsShadow     int    `json:"facts_shadow"`
	FactsPending    int    `json:"facts_pending"`
	Complete        bool   `json:"complete"`
}

// Progress reports how much of a namespace has a shadow embedding.
//
// Counts only rows that actually have a live embedding. A row with no live
// vector was never semantically retrievable, so counting it as "pending" would
// make a finished migration look permanently incomplete.
func (s *ShadowMigrator) Progress(ctx context.Context, namespaceSlug string) (ShadowProgress, error) {
	p := ShadowProgress{Namespace: namespaceSlug}

	nsFilter, args := "", []any{}
	if namespaceSlug != "" && namespaceSlug != "/" {
		nsFilter = " AND namespace_id = (SELECT id FROM namespaces WHERE slug = $1)"
		args = append(args, namespaceSlug)
	}

	for _, t := range []struct {
		table                  string
		total, shadow, pending *int
	}{
		{"episodes", &p.EpisodesTotal, &p.EpisodesShadow, &p.EpisodesPending},
		{"facts", &p.FactsTotal, &p.FactsShadow, &p.FactsPending},
	} {
		q := fmt.Sprintf(`
			SELECT COUNT(*),
			       COUNT(embedding_shadow),
			       COUNT(*) FILTER (WHERE embedding_shadow IS NULL)
			  FROM %s
			 WHERE embedding IS NOT NULL AND deleted_at IS NULL%s`, t.table, nsFilter)
		if err := s.pool.QueryRow(ctx, q, args...).Scan(t.total, t.shadow, t.pending); err != nil {
			return p, fmt.Errorf("shadow: count %s: %w", t.table, err)
		}
	}

	p.Complete = p.EpisodesPending == 0 && p.FactsPending == 0
	return p, nil
}

// ShadowWaveResult is what one bounded wave actually did.
type ShadowWaveResult struct {
	Namespace string   `json:"namespace"`
	Embedded  int      `json:"embedded"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors,omitempty"`
	Remaining int      `json:"remaining"`
	Complete  bool     `json:"complete"`
}

// MigrateWave embeds up to one batch of pending rows in a namespace.
//
// Bounded by construction: one call does at most `batch` provider requests and
// then returns, so a wave can never become an unattended spend loop. Drive it
// from a loop that re-checks Progress between calls.
//
// Fails CLOSED on provider trouble: the first embedder error that indicates the
// provider is unavailable aborts the wave immediately rather than grinding
// through the batch accumulating failures and cost.
func (s *ShadowMigrator) MigrateWave(ctx context.Context, namespaceSlug string) (ShadowWaveResult, error) {
	res := ShadowWaveResult{Namespace: namespaceSlug}

	nsFilter, args := "", []any{}
	if namespaceSlug != "" && namespaceSlug != "/" {
		nsFilter = " AND namespace_id = (SELECT id FROM namespaces WHERE slug = $1)"
		args = append(args, namespaceSlug)
	}
	args = append(args, s.batch)
	limitPos := len(args)

	// Only rows that already have a live embedding are candidates: the shadow
	// column mirrors the live representation, it does not extend coverage.
	q := fmt.Sprintf(`
		SELECT memory_table, id, content FROM (
			SELECT 'episodes'::text AS memory_table, id, content, created_at
			  FROM episodes
			 WHERE embedding_shadow IS NULL AND embedding IS NOT NULL AND deleted_at IS NULL%s
			UNION ALL
			SELECT 'facts'::text AS memory_table, id, content, created_at
			  FROM facts
			 WHERE embedding_shadow IS NULL AND embedding IS NOT NULL AND deleted_at IS NULL%s
		) pending
		ORDER BY created_at, id
		LIMIT $%d`, nsFilter, nsFilter, limitPos)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("shadow: query pending: %w", err)
	}
	type candidate struct {
		table   string
		id      int64
		content string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.table, &c.id, &c.content); err != nil {
			rows.Close()
			return res, fmt.Errorf("shadow: scan pending: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, fmt.Errorf("shadow: pending rows: %w", err)
	}
	rows.Close()

	for _, c := range candidates {
		if ctx.Err() != nil {
			break
		}
		vec, err := s.embedder.Embed(ctx, c.content)
		if err != nil {
			// Circuit open / quota / payment: stop the wave now. Continuing
			// would burn the rest of the batch against a provider we already
			// know is refusing us.
			if embedder.IsUnavailable(err) {
				res.Errors = append(res.Errors, fmt.Sprintf("provider unavailable, wave aborted: %v", err))
				break
			}
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s %d: %v", c.table, c.id, err))
			continue
		}
		if len(vec) != s.dims {
			// Defence in depth. The embedder validates this too, but a shadow
			// column with mixed widths is unrecoverable, so refuse loudly.
			res.Errors = append(res.Errors, fmt.Sprintf("%s %d: provider returned %d dims, want %d", c.table, c.id, len(vec), s.dims))
			res.Failed++
			continue
		}

		// The IS NULL guard makes concurrent wave drivers idempotent rather
		// than double-spending on the same row.
		stmt := fmt.Sprintf(
			"UPDATE %s SET embedding_shadow = $1, embedding_shadow_model = $2 WHERE id = $3 AND embedding_shadow IS NULL",
			c.table,
		)
		if _, err := s.pool.Exec(ctx, stmt, pgvector.NewVector(vec), s.model, c.id); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("write %s %d: %v", c.table, c.id, err))
			continue
		}
		res.Embedded++
	}

	progress, err := s.Progress(ctx, namespaceSlug)
	if err != nil {
		return res, err
	}
	res.Remaining = progress.EpisodesPending + progress.FactsPending
	res.Complete = progress.Complete
	return res, nil
}

// compareIDSets reports how two ranked result sets differ.
//
// Kept pure and separate from the SQL so the arithmetic behind a migration
// go/no-go can be tested exactly, without a database. Ratio is measured
// against the LIVE set: the question being answered is "how much of what
// production returns today would the new model still return", not the
// symmetric similarity of two arbitrary sets.
func compareIDSets(liveIDs, shadowIDs []int64) (overlap int, liveOnly, shadowOnly []int64, ratio float64) {
	live := make(map[int64]bool, len(liveIDs))
	for _, id := range liveIDs {
		live[id] = true
	}
	shadow := make(map[int64]bool, len(shadowIDs))
	for _, id := range shadowIDs {
		shadow[id] = true
	}
	for _, id := range liveIDs {
		if shadow[id] {
			overlap++
		} else {
			liveOnly = append(liveOnly, id)
		}
	}
	for _, id := range shadowIDs {
		if !live[id] {
			shadowOnly = append(shadowOnly, id)
		}
	}
	if n := len(liveIDs); n > 0 {
		ratio = float64(overlap) / float64(n)
	}
	return overlap, liveOnly, shadowOnly, ratio
}

// ShadowComparison is the evidence for a go/no-go on the read-path swap.
type ShadowComparison struct {
	Query        string   `json:"query"`
	Namespace    string   `json:"namespace"`
	LiveIDs      []int64  `json:"live_fact_ids"`
	ShadowIDs    []int64  `json:"shadow_fact_ids"`
	Overlap      int      `json:"overlap"`
	OverlapRatio float64  `json:"overlap_ratio"`
	LiveOnly     []int64  `json:"live_only"`
	ShadowOnly   []int64  `json:"shadow_only"`
	Notes        []string `json:"notes,omitempty"`
}

// Compare runs one query against both representations and reports the overlap.
//
// This is the validation gate. It deliberately reports raw ID sets rather than
// a pass/fail verdict: overlap is evidence a human interprets, not a threshold
// a migration should silently clear itself against. Low overlap is not
// automatically bad — a better model SHOULD disagree with a worse one
// sometimes — which is exactly why this returns the disagreement instead of
// scoring it.
func (s *ShadowMigrator) Compare(ctx context.Context, liveEmbedder embedder.Embedder, namespaceSlug, query string, limit int) (ShadowComparison, error) {
	cmp := ShadowComparison{Query: query, Namespace: namespaceSlug}
	if limit <= 0 {
		limit = 10
	}

	liveVec, err := embedder.EmbedQuery(ctx, liveEmbedder, query)
	if err != nil {
		return cmp, fmt.Errorf("shadow: embed query with live model: %w", err)
	}
	shadowVec, err := embedder.EmbedQuery(ctx, s.embedder, query)
	if err != nil {
		return cmp, fmt.Errorf("shadow: embed query with shadow model: %w", err)
	}

	nsFilter, baseArgs := "", []any{}
	if namespaceSlug != "" && namespaceSlug != "/" {
		nsFilter = " AND namespace_id = (SELECT id FROM namespaces WHERE slug = $3)"
		baseArgs = append(baseArgs, namespaceSlug)
	}

	topIDs := func(column string, vec []float32) ([]int64, error) {
		q := fmt.Sprintf(`
			SELECT id FROM facts
			 WHERE %s IS NOT NULL AND deleted_at IS NULL%s
			 ORDER BY %s <=> $1
			 LIMIT $2`, column, nsFilter, column)
		args := append([]any{pgvector.NewVector(vec), limit}, baseArgs...)
		rows, err := s.pool.Query(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}

	if cmp.LiveIDs, err = topIDs("embedding", liveVec); err != nil {
		return cmp, fmt.Errorf("shadow: live query: %w", err)
	}
	if cmp.ShadowIDs, err = topIDs("embedding_shadow", shadowVec); err != nil {
		return cmp, fmt.Errorf("shadow: shadow query: %w", err)
	}

	cmp.Overlap, cmp.LiveOnly, cmp.ShadowOnly, cmp.OverlapRatio = compareIDSets(cmp.LiveIDs, cmp.ShadowIDs)

	if len(cmp.ShadowIDs) < len(cmp.LiveIDs) {
		cmp.Notes = append(cmp.Notes,
			"shadow returned fewer results than live — migration is probably incomplete for this namespace; re-check Progress before reading anything into the overlap")
	}
	return cmp, nil
}
