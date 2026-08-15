package brain

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/alash3al/stash/internal/embedder"
	"github.com/alash3al/stash/internal/queries"
	"github.com/alash3al/stash/internal/reasoner"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNamespaceNotFound = fmt.Errorf("brain: namespace not found — call create_namespace first")
	ErrEpisodeNotFound   = fmt.Errorf("brain: episode not found")
	ErrFactNotFound      = fmt.Errorf("brain: fact not found")
	ErrEmptyContent      = fmt.Errorf("brain: content cannot be empty")
	ErrContentTooLong    = fmt.Errorf("brain: content exceeds maximum length")
	ErrInvalidPath       = fmt.Errorf("brain: namespace path must start with / and contain valid segments (lowercase alphanumeric, hyphens, underscores)")

	pathSegmentRe         = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	ErrNamespacesRequired = fmt.Errorf("brain: at least one namespace is required")
	maxContentLen         = 10000
)

const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// Pagination controls offset-based pagination for list queries.
type Pagination struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// Sanitize applies defaults and enforces bounds.
func (p Pagination) Sanitize() Pagination {
	if p.Limit <= 0 {
		p.Limit = DefaultLimit
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

type Config struct {
	BatchSize                      int
	SimilarityThreshold            float64
	DedupThreshold                 float64
	Window                         time.Duration
	DecayFactor                    float64
	ExpiryThreshold                float32
	HypothesisAutoConfirmThreshold float32
	HypothesisAutoRejectThreshold  float32
	RetrievalLearningEnabled       bool
	RetrievalOverfetchFactor       int
	RetrievalUtilityWeight         float64
	RetrievalMaxUtilityDelta       float64
	RecallHistoryRetention         time.Duration
	EmbeddingBackfillBatch         int
	ProviderReranker               ProviderReranker
	ProviderRerankCandidateLimit   int

	// MaxRecordAttempts bounds how many times one record may fail a stage for a
	// permanent reason before the watermark is allowed past it. Zero uses
	// DefaultMaxRecordAttempts.
	MaxRecordAttempts int

	// CycleReasonerCallCeiling caps reasoner calls per namespace per cycle
	// regardless of watermark state. Zero uses DefaultCycleReasonerCallCeiling.
	CycleReasonerCallCeiling int
}

func DefaultConfig() Config {
	return Config{
		BatchSize:                      100,
		SimilarityThreshold:            0.85,
		DedupThreshold:                 0.85,
		Window:                         7 * 24 * time.Hour,
		DecayFactor:                    0.95,
		ExpiryThreshold:                0.1,
		HypothesisAutoConfirmThreshold: 0.9,
		HypothesisAutoRejectThreshold:  0.9,
		RetrievalLearningEnabled:       false,
		RetrievalOverfetchFactor:       3,
		RetrievalUtilityWeight:         0.08,
		RetrievalMaxUtilityDelta:       0.10,
		RecallHistoryRetention:         90 * 24 * time.Hour,
		EmbeddingBackfillBatch:         25,
		ProviderRerankCandidateLimit:   50,
		MaxRecordAttempts:              DefaultMaxRecordAttempts,
		CycleReasonerCallCeiling:       DefaultCycleReasonerCallCeiling,
	}
}

type Brain struct {
	pool     *pgxpool.Pool
	embedder embedder.Embedder
	reasoner reasoner.Reasoner
	queries  *queries.Queries
	config   Config
}

func New(pool *pgxpool.Pool, e embedder.Embedder, r reasoner.Reasoner, q *queries.Queries, cfg Config) (*Brain, error) {
	if pool == nil {
		return nil, fmt.Errorf("brain: pool is required")
	}
	if e == nil {
		return nil, fmt.Errorf("brain: embedder is required")
	}
	if r == nil {
		return nil, fmt.Errorf("brain: reasoner is required")
	}
	if q == nil {
		return nil, fmt.Errorf("brain: queries is required")
	}
	if cfg.RetrievalOverfetchFactor <= 0 {
		cfg.RetrievalOverfetchFactor = 3
	}
	if cfg.RetrievalUtilityWeight < 0 || cfg.RetrievalUtilityWeight > 1 {
		return nil, fmt.Errorf("brain: retrieval utility weight must be between 0 and 1")
	}
	if cfg.RetrievalMaxUtilityDelta < 0 || cfg.RetrievalMaxUtilityDelta > 1 {
		return nil, fmt.Errorf("brain: retrieval max utility delta must be between 0 and 1")
	}
	if cfg.RecallHistoryRetention <= 0 {
		cfg.RecallHistoryRetention = 90 * 24 * time.Hour
	}
	if cfg.EmbeddingBackfillBatch <= 0 {
		cfg.EmbeddingBackfillBatch = 25
	}
	if cfg.ProviderRerankCandidateLimit <= 0 {
		cfg.ProviderRerankCandidateLimit = 50
	}
	return &Brain{
		pool:     pool,
		embedder: e,
		reasoner: r,
		queries:  q,
		config:   cfg,
	}, nil
}

func (b *Brain) Close() {
	b.pool.Close()
}

// consolidationBatchLimit applies the operator-configured batch size to every
// LLM-bearing stage. Historically only episode synthesis honored BatchSize;
// relationship extraction could still make 50 sequential provider calls and
// monopolize the fleet for minutes.
func (b *Brain) consolidationBatchLimit(stageMaximum int) int {
	limit := b.config.BatchSize
	if limit <= 0 {
		limit = DefaultConfig().BatchSize
	}
	if stageMaximum > 0 && limit > stageMaximum {
		limit = stageMaximum
	}
	return limit
}

func (b *Brain) consolidationBatchLimitAtLeast(stageMaximum, minimum int) int {
	limit := b.consolidationBatchLimit(stageMaximum)
	if limit < minimum {
		return minimum
	}
	return limit
}

func validateContent(content string) error {
	if content == "" {
		return ErrEmptyContent
	}
	if len(content) > maxContentLen {
		return ErrContentTooLong
	}
	return nil
}

func validatePath(path string) error {
	if path == "" || path == "/" {
		return nil
	}
	if !strings.HasPrefix(path, "/") {
		return ErrInvalidPath
	}
	segments := splitPath(path)
	for _, seg := range segments {
		if !pathSegmentRe.MatchString(seg) {
			return ErrInvalidPath
		}
	}
	return nil
}

// splitPath splits a path into its segments.
// "/" → [], "/foo/bar" → ["foo", "bar"]
func splitPath(path string) []string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// escapeLikePattern escapes PostgreSQL LIKE meta-characters so the input
// is matched as a literal. Use with `ESCAPE '\'` in the query.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func escapeLikePattern(s string) string {
	return likeEscaper.Replace(s)
}

// likePatternForDescendants builds a LIKE prefix for slug descendant lookup.
func likePatternForDescendants(path string) string {
	return escapeLikePattern(path) + "/%"
}

// resolveNamespaceID returns the namespace ID for an exact path.
// Returns ErrNamespaceNotFound if no matching namespace exists.
func (b *Brain) resolveNamespaceID(ctx context.Context, path string) (int64, error) {
	var id int64
	err := b.pool.QueryRow(ctx,
		"SELECT id FROM namespaces WHERE slug = $1", path,
	).Scan(&id)
	if err != nil {
		return 0, ErrNamespaceNotFound
	}
	return id, nil
}

// ResolveNamespaceIDs resolves a list of paths to namespace IDs.
// Each path matches itself and all descendants. For "/", returns all IDs.
// Returns ErrNamespacesRequired if paths is empty.
func (b *Brain) ResolveNamespaceIDs(ctx context.Context, paths []string) ([]int64, error) {
	return b.resolveNamespaceIDs(ctx, paths)
}

// resolveNamespaceIDs resolves a list of paths to namespace IDs.
// Each path matches itself and all descendants.
func (b *Brain) resolveNamespaceIDs(ctx context.Context, paths []string) ([]int64, error) {
	if len(paths) == 0 {
		return nil, ErrNamespacesRequired
	}

	var allIDs []int64
	seen := make(map[int64]bool)

	for _, p := range paths {
		if err := validatePath(p); err != nil {
			return nil, fmt.Errorf("namespace %q: %w", p, err)
		}
		expanded, err := b.resolveNamespaceIDWithDescendants(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("namespace %q: %w", p, err)
		}
		for _, id := range expanded {
			if !seen[id] {
				seen[id] = true
				allIDs = append(allIDs, id)
			}
		}
	}

	return allIDs, nil
}

// resolveNamespaceIDWithDescendants returns IDs for the exact path plus all descendants.
// For "/", returns all namespace IDs.
func (b *Brain) resolveNamespaceIDWithDescendants(ctx context.Context, path string) ([]int64, error) {
	if path == "/" {
		rows, err := b.pool.Query(ctx, "SELECT id FROM namespaces")
		if err != nil {
			return nil, fmt.Errorf("resolve all namespaces: %w", err)
		}
		defer rows.Close()

		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan namespace: %w", err)
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}

	// Slug segments allow `_` (see pathSegmentRe), which is a LIKE wildcard
	// in PostgreSQL. Escape it so `/foo_bar` only matches its own descendants,
	// not `/fooXbar/...` for any X.
	rows, err := b.pool.Query(ctx,
		`SELECT id FROM namespaces WHERE slug = $1 OR slug LIKE $2 ESCAPE '\'`,
		path, likePatternForDescendants(path),
	)
	if err != nil {
		return nil, fmt.Errorf("resolve namespace descendants: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan namespace: %w", err)
		}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		return nil, ErrNamespaceNotFound
	}

	return ids, rows.Err()
}

func (b *Brain) Health(ctx context.Context) error {
	return b.pool.Ping(ctx)
}

func (b *Brain) Ready(ctx context.Context) error {
	_, err := b.pool.Exec(ctx, "SELECT 1 FROM consolidation_progress LIMIT 0")
	if err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	if err := b.pool.Ping(ctx); err != nil {
		return err
	}
	if err := embedder.Availability(b.embedder); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	return nil
}
