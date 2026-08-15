package brain

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/alash3al/stash/internal/observability"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// RecallResult is a unified result from semantic search across episodes and facts.
type RecallResult struct {
	ID                  int64   `json:"id"`
	NamespaceID         int64   `json:"namespace_id"`
	Content             string  `json:"content"`
	Confidence          float32 `json:"confidence,omitempty"`
	Score               float32 `json:"score"`
	SemanticScore       float32 `json:"semantic_score,omitempty"`
	UtilityScore        float32 `json:"utility_score,omitempty"`
	ImpressionID        int64   `json:"impression_id,omitempty"`
	Type                string  `json:"type"`
	OccurredAt          string  `json:"occurred_at,omitempty"`
	ValidFrom           string  `json:"valid_from,omitempty"`
	CreatedAt           string  `json:"created_at"`
	RetrievalMode       string  `json:"retrieval_mode,omitempty"`
	ProviderRerankScore float32 `json:"provider_rerank_score,omitempty"`
}

// RecallOptions controls the additive learning ledger. Zero-value options keep
// callers private and do not change ranking unless the Brain config enables it.
type RecallOptions struct {
	RecordOutcome bool
	Caller        string
}

type FeedbackResult struct {
	Recorded     bool    `json:"recorded"`
	UtilityScore float32 `json:"utility_score"`
	HelpfulCount int64   `json:"helpful_count"`
	HarmfulCount int64   `json:"harmful_count"`
	NeutralCount int64   `json:"neutral_count"`
}

var (
	ErrInvalidFeedbackSignal = fmt.Errorf("brain: feedback signal must be helpful, harmful, or neutral")
	ErrInvalidMemoryType     = fmt.Errorf("brain: memory type must be fact or episode")
	ErrIdempotencyRequired   = fmt.Errorf("brain: feedback idempotency key is required")
	ErrIdempotencyKeyReuse   = fmt.Errorf("brain: feedback idempotency key was already used for a different result")
)

// Recall searches episodes and facts by semantic similarity across the given namespaces.
// Each namespace path matches itself and all descendants. Namespaces is required.
func (b *Brain) Recall(ctx context.Context, namespaces []string, query string, limit int) ([]RecallResult, error) {
	return b.RecallWithOptions(ctx, namespaces, query, limit, RecallOptions{
		RecordOutcome: b.config.RetrievalLearningEnabled,
		Caller:        "brain",
	})
}

// RecallWithOptions searches facts and episodes, optionally recording a
// privacy-preserving impression and applying bounded utility reranking.
func (b *Brain) RecallWithOptions(ctx context.Context, namespaces []string, query string, limit int, opts RecallOptions) ([]RecallResult, error) {
	if err := validateContent(query); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	nsIDs, err := b.resolveNamespaceIDs(ctx, namespaces)
	if err != nil {
		return nil, err
	}

	vec, err := b.embedder.Embed(ctx, query)
	if err != nil {
		return b.recallLexical(ctx, nsIDs, query, limit)
	}

	pgVec := pgvector.NewVector(vec)

	learningEnabled := b.config.RetrievalLearningEnabled && opts.RecordOutcome
	candidateLimit := limit
	providerRerankEnabled := b.config.ProviderReranker != nil
	if providerRerankEnabled && candidateLimit < b.config.ProviderRerankCandidateLimit {
		candidateLimit = b.config.ProviderRerankCandidateLimit
	}
	if learningEnabled {
		candidateLimit = limit * b.config.RetrievalOverfetchFactor
		if candidateLimit > 300 {
			candidateLimit = 300
		}
	}

	// In learning mode both types are over-fetched before the unified rerank.
	// With the feature disabled, preserve the legacy facts-first behavior.
	factLimit := candidateLimit
	factSQL, factArgs, err := b.queries.RecallFacts(nsIDs, pgVec, factLimit)
	if err != nil {
		return nil, fmt.Errorf("build fact query: %w", err)
	}

	factRows, err := b.pool.Query(ctx, factSQL, factArgs...)
	if err != nil {
		return nil, fmt.Errorf("query facts: %w", err)
	}
	defer factRows.Close()

	var results []RecallResult
	for factRows.Next() {
		var id int64
		var namespaceID int64
		var content string
		var confidence float32
		var validFrom *time.Time
		var score float32
		var createdAt time.Time

		if err := factRows.Scan(&id, &namespaceID, &content, &confidence, &validFrom, &createdAt, &score); err != nil {
			return nil, fmt.Errorf("scan fact: %w", err)
		}
		results = append(results, RecallResult{
			ID:            id,
			NamespaceID:   namespaceID,
			Content:       content,
			Confidence:    confidence,
			Score:         score,
			SemanticScore: score,
			Type:          "fact",
			RetrievalMode: "semantic",
			CreatedAt:     createdAt.Format(time.RFC3339),
		})
		if validFrom != nil {
			results[len(results)-1].ValidFrom = validFrom.Format(time.RFC3339)
		}
	}
	if err := factRows.Err(); err != nil {
		return nil, fmt.Errorf("fact rows: %w", err)
	}

	// Search episodes for remaining slots
	episodeLimit := limit - len(results)
	if learningEnabled || providerRerankEnabled {
		episodeLimit = candidateLimit
	}
	if episodeLimit > 0 {
		epSQL, epArgs, err := b.queries.RecallEpisodes(nsIDs, pgVec, episodeLimit)
		if err != nil {
			return nil, fmt.Errorf("build episode query: %w", err)
		}

		epRows, err := b.pool.Query(ctx, epSQL, epArgs...)
		if err != nil {
			return nil, fmt.Errorf("query episodes: %w", err)
		}
		defer epRows.Close()

		for epRows.Next() {
			var id int64
			var namespaceID int64
			var content string
			var score float32
			var occurredAt time.Time
			var createdAt time.Time

			if err := epRows.Scan(&id, &namespaceID, &content, &occurredAt, &createdAt, &score); err != nil {
				return nil, fmt.Errorf("scan episode: %w", err)
			}
			results = append(results, RecallResult{
				ID:            id,
				NamespaceID:   namespaceID,
				Content:       content,
				Score:         score,
				SemanticScore: score,
				Type:          "episode",
				RetrievalMode: "semantic",
				OccurredAt:    occurredAt.Format(time.RFC3339),
				CreatedAt:     createdAt.Format(time.RFC3339),
			})
		}
		if err := epRows.Err(); err != nil {
			return nil, fmt.Errorf("episode rows: %w", err)
		}
	}

	if learningEnabled {
		utilities, err := b.loadUtilities(ctx, results)
		if err != nil {
			// Utility is an optimization, not an availability dependency. Preserve
			// semantic recall when its side table is temporarily unavailable.
			observability.RecordRecallLearningError("load_utility")
		} else {
			rerankResults(results, utilities, b.config.RetrievalUtilityWeight, b.config.RetrievalMaxUtilityDelta)
		}
	}

	if providerRerankEnabled && len(results) > 0 {
		documents := make([]string, len(results))
		for i := range results {
			documents[i] = results[i].Content
		}
		scores, err := b.config.ProviderReranker.Rerank(ctx, query, documents)
		if err != nil {
			// Provider ranking is an optimization, never an availability dependency.
			// Preserve BGE-M3 semantic ranking and make the fallback visible.
			for i := range results {
				results[i].RetrievalMode = "semantic+provider_rerank_unavailable"
			}
		} else {
			for i := range results {
				results[i].ProviderRerankScore = scores[i]
				results[i].Score = scores[i]
				results[i].RetrievalMode = "semantic+provider_rerank"
			}
		}
	}

	sortRecallResults(results)

	if len(results) > limit {
		results = results[:limit]
	}

	if learningEnabled && len(results) > 0 {
		impressionID, err := b.recordRecallImpression(ctx, nsIDs, query, opts.Caller, results)
		if err != nil {
			observability.RecordRecallLearningError("record_impression")
		} else {
			for i := range results {
				results[i].ImpressionID = impressionID
			}
		}
	}

	observability.RecordRecall(len(results), learningEnabled)
	return results, nil
}

// recallLexical is the availability fallback for provider outages. It uses
// PostgreSQL full-text ranking and therefore keeps recall useful without an
// embedding call. The retrieval_mode field makes degradation explicit.
func (b *Brain) recallLexical(ctx context.Context, nsIDs []int64, query string, limit int) ([]RecallResult, error) {
	rows, err := b.pool.Query(ctx, `
		WITH q AS (SELECT websearch_to_tsquery('english', $1) AS value), candidates AS (
			SELECT f.id, f.namespace_id, f.content, f.confidence,
			       NULL::timestamptz AS occurred_at, f.valid_from, f.created_at,
			       ts_rank_cd(to_tsvector('english', f.content), q.value) AS score,
			       'fact'::text AS memory_type
			FROM facts f, q
			WHERE f.namespace_id = ANY($2) AND f.deleted_at IS NULL
			  AND (f.valid_until IS NULL OR f.valid_until > now())
			  AND to_tsvector('english', f.content) @@ q.value
			UNION ALL
			SELECT e.id, e.namespace_id, e.content, NULL::real AS confidence,
			       e.occurred_at, NULL::timestamptz AS valid_from, e.created_at,
			       ts_rank_cd(to_tsvector('english', e.content), q.value) AS score,
			       'episode'::text AS memory_type
			FROM episodes e, q
			WHERE e.namespace_id = ANY($2) AND e.deleted_at IS NULL
			  AND to_tsvector('english', e.content) @@ q.value
		)
		SELECT id, namespace_id, content, confidence, occurred_at, valid_from, created_at, score, memory_type
		FROM candidates ORDER BY score DESC, created_at DESC LIMIT $3`, query, nsIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("lexical recall: %w", err)
	}
	defer rows.Close()

	results := make([]RecallResult, 0, limit)
	for rows.Next() {
		var result RecallResult
		var confidence *float32
		var occurredAt, validFrom *time.Time
		var createdAt time.Time
		if err := rows.Scan(&result.ID, &result.NamespaceID, &result.Content, &confidence, &occurredAt, &validFrom, &createdAt, &result.Score, &result.Type); err != nil {
			return nil, fmt.Errorf("scan lexical recall: %w", err)
		}
		if confidence != nil {
			result.Confidence = *confidence
		}
		if occurredAt != nil {
			result.OccurredAt = occurredAt.Format(time.RFC3339)
		}
		if validFrom != nil {
			result.ValidFrom = validFrom.Format(time.RFC3339)
		}
		result.CreatedAt = createdAt.Format(time.RFC3339)
		result.RetrievalMode = "lexical_degraded"
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lexical recall rows: %w", err)
	}
	observability.RecordDegradedRecall(len(results))
	observability.RecordRecall(len(results), false)
	return results, nil
}

type memoryRef struct {
	Type string
	ID   int64
}

func (b *Brain) loadUtilities(ctx context.Context, results []RecallResult) (map[memoryRef]float32, error) {
	var factIDs, episodeIDs []int64
	for _, result := range results {
		if result.Type == "fact" {
			factIDs = append(factIDs, result.ID)
		} else if result.Type == "episode" {
			episodeIDs = append(episodeIDs, result.ID)
		}
	}
	rows, err := b.pool.Query(ctx,
		`SELECT memory_type, memory_id, utility_score FROM memory_utility
		 WHERE (memory_type = 'fact' AND memory_id = ANY($1))
		    OR (memory_type = 'episode' AND memory_id = ANY($2))`,
		factIDs, episodeIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("load memory utility: %w", err)
	}
	defer rows.Close()
	utilities := make(map[memoryRef]float32)
	for rows.Next() {
		var ref memoryRef
		var score float32
		if err := rows.Scan(&ref.Type, &ref.ID, &score); err != nil {
			return nil, fmt.Errorf("scan memory utility: %w", err)
		}
		utilities[ref] = score
	}
	return utilities, rows.Err()
}

func rerankResults(results []RecallResult, utilities map[memoryRef]float32, weight, maxDelta float64) {
	for i := range results {
		utility := float64(utilities[memoryRef{Type: results[i].Type, ID: results[i].ID}])
		utility = clamp(utility, -1, 1)
		delta := clamp(utility*weight, -maxDelta, maxDelta)
		results[i].UtilityScore = float32(utility)
		// Final score is semantic similarity plus a separately exposed, bounded
		// delta. Do not clamp it back to cosine's [-1,1] range: saturation at 1
		// would make a perfect semantic match permanently unbeatable and erase
		// the learning signal. SemanticScore remains the canonical similarity.
		results[i].Score = float32(float64(results[i].SemanticScore) + delta)
	}
}

func sortRecallResults(results []RecallResult) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].SemanticScore != results[j].SemanticScore {
			return results[i].SemanticScore > results[j].SemanticScore
		}
		if results[i].Confidence != results[j].Confidence {
			return results[i].Confidence > results[j].Confidence
		}
		if results[i].CreatedAt != results[j].CreatedAt {
			return results[i].CreatedAt > results[j].CreatedAt
		}
		if results[i].Type != results[j].Type {
			return results[i].Type == "fact"
		}
		return results[i].ID < results[j].ID
	})
}

func clamp(value, low, high float64) float64 {
	return math.Max(low, math.Min(high, value))
}

func (b *Brain) recordRecallImpression(ctx context.Context, namespaceIDs []int64, query, caller string, results []RecallResult) (int64, error) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(query)))
	caller = strings.TrimSpace(caller)
	if caller == "" {
		caller = "unknown"
	}
	if len(caller) > 64 {
		caller = caller[:64]
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin recall impression: %w", err)
	}
	defer tx.Rollback(ctx)

	var impressionID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO recall_impressions (namespace_ids, query_hash, caller, result_count)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		namespaceIDs, hash, caller, len(results),
	).Scan(&impressionID); err != nil {
		return 0, fmt.Errorf("insert recall impression: %w", err)
	}

	for rank, result := range results {
		if _, err := tx.Exec(ctx,
			`INSERT INTO recall_impression_results
			 (impression_id, memory_type, memory_id, namespace_id, rank, semantic_score, utility_score, final_score)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			impressionID, result.Type, result.ID, result.NamespaceID, rank+1,
			result.SemanticScore, result.UtilityScore, result.Score,
		); err != nil {
			return 0, fmt.Errorf("insert recall impression result: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO memory_utility
			 (memory_type, memory_id, namespace_id, recall_count, last_recalled_at)
			 VALUES ($1, $2, $3, 1, now())
			 ON CONFLICT (memory_type, memory_id) DO UPDATE SET
			   recall_count = memory_utility.recall_count + 1,
			   last_recalled_at = now(), updated_at = now()`,
			result.Type, result.ID, result.NamespaceID,
		); err != nil {
			return 0, fmt.Errorf("update memory recall utility: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit recall impression: %w", err)
	}
	return impressionID, nil
}

// RecordRecallFeedback appends one idempotent outcome and updates retrieval
// utility. It never updates facts.confidence, validity, contradictions, or any
// canonical lifecycle field.
func (b *Brain) RecordRecallFeedback(ctx context.Context, impressionID int64, memoryType string, memoryID int64, signal, idempotencyKey, reason string) (FeedbackResult, error) {
	if memoryType != "fact" && memoryType != "episode" {
		return FeedbackResult{}, ErrInvalidMemoryType
	}
	if signal != "helpful" && signal != "harmful" && signal != "neutral" {
		return FeedbackResult{}, ErrInvalidFeedbackSignal
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return FeedbackResult{}, ErrIdempotencyRequired
	}
	if len(idempotencyKey) > 200 {
		return FeedbackResult{}, fmt.Errorf("brain: feedback idempotency key exceeds 200 characters")
	}
	if len(reason) > 1000 {
		return FeedbackResult{}, fmt.Errorf("brain: feedback reason exceeds 1000 characters")
	}

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return FeedbackResult{}, fmt.Errorf("begin recall feedback: %w", err)
	}
	defer tx.Rollback(ctx)

	var feedbackID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO recall_feedback
		 (impression_id, memory_type, memory_id, signal, idempotency_key, reason)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		 ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`,
		impressionID, memoryType, memoryID, signal, idempotencyKey, reason,
	).Scan(&feedbackID)
	if err != nil && err != pgx.ErrNoRows {
		return FeedbackResult{}, fmt.Errorf("insert recall feedback: %w", err)
	}
	if err == pgx.ErrNoRows {
		var existingImpressionID, existingMemoryID int64
		var existingMemoryType string
		if lookupErr := tx.QueryRow(ctx,
			`SELECT impression_id, memory_type, memory_id FROM recall_feedback
			 WHERE idempotency_key = $1`, idempotencyKey,
		).Scan(&existingImpressionID, &existingMemoryType, &existingMemoryID); lookupErr != nil {
			return FeedbackResult{}, fmt.Errorf("query existing feedback idempotency key: %w", lookupErr)
		}
		if existingImpressionID != impressionID || existingMemoryType != memoryType || existingMemoryID != memoryID {
			return FeedbackResult{}, ErrIdempotencyKeyReuse
		}
		result, queryErr := queryFeedbackResult(ctx, tx, memoryType, memoryID)
		if queryErr != nil {
			return FeedbackResult{}, queryErr
		}
		result.Recorded = false
		return result, nil
	}

	helpful, harmful, neutral := 0, 0, 0
	switch signal {
	case "helpful":
		helpful = 1
	case "harmful":
		harmful = 1
	case "neutral":
		neutral = 1
	}
	var result FeedbackResult
	err = tx.QueryRow(ctx,
		`INSERT INTO memory_utility
		 (memory_type, memory_id, namespace_id, helpful_count, harmful_count, neutral_count, last_feedback_at)
		 SELECT $2, $3, namespace_id, $4, $5, $6, now()
		 FROM recall_impression_results
		 WHERE impression_id = $1 AND memory_type = $2 AND memory_id = $3
		 ON CONFLICT (memory_type, memory_id) DO UPDATE SET
		   helpful_count = memory_utility.helpful_count + EXCLUDED.helpful_count,
		   harmful_count = memory_utility.harmful_count + EXCLUDED.harmful_count,
		   neutral_count = memory_utility.neutral_count + EXCLUDED.neutral_count,
		   utility_score = (
		     (memory_utility.helpful_count + EXCLUDED.helpful_count)::REAL -
		     (memory_utility.harmful_count + EXCLUDED.harmful_count)::REAL
		   ) / (
		     memory_utility.helpful_count + EXCLUDED.helpful_count +
		     memory_utility.harmful_count + EXCLUDED.harmful_count + 4
		   ),
		   last_feedback_at = now(), updated_at = now()
		 RETURNING utility_score, helpful_count, harmful_count, neutral_count`,
		impressionID, memoryType, memoryID, helpful, harmful, neutral,
	).Scan(&result.UtilityScore, &result.HelpfulCount, &result.HarmfulCount, &result.NeutralCount)
	if err != nil {
		return FeedbackResult{}, fmt.Errorf("update memory utility: %w", err)
	}
	result.Recorded = true
	if err := tx.Commit(ctx); err != nil {
		return FeedbackResult{}, fmt.Errorf("commit recall feedback: %w", err)
	}
	observability.RecordRecallFeedback(signal)
	return result, nil
}

func queryFeedbackResult(ctx context.Context, tx pgx.Tx, memoryType string, memoryID int64) (FeedbackResult, error) {
	var result FeedbackResult
	err := tx.QueryRow(ctx,
		`SELECT utility_score, helpful_count, harmful_count, neutral_count
		 FROM memory_utility WHERE memory_type = $1 AND memory_id = $2`,
		memoryType, memoryID,
	).Scan(&result.UtilityScore, &result.HelpfulCount, &result.HarmfulCount, &result.NeutralCount)
	if err != nil {
		return FeedbackResult{}, fmt.Errorf("query memory utility: %w", err)
	}
	return result, nil
}

// PruneRecallHistory bounds the append-only impression ledger. Aggregated
// utility survives pruning, while individual feedback and candidates cascade.
func (b *Brain) PruneRecallHistory(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-b.config.RecallHistoryRetention)
	result, err := b.pool.Exec(ctx, "DELETE FROM recall_impressions WHERE created_at < $1", cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune recall history: %w", err)
	}
	return result.RowsAffected(), nil
}
