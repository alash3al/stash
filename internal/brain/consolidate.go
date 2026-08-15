package brain

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/alash3al/stash/internal/observability"
	"github.com/pgvector/pgvector-go"
)

var ErrConsolidationInProgress = fmt.Errorf("brain: consolidation already in progress for namespace")

// ConsolidationResult describes the outcome of a consolidation run.
type ConsolidationResult struct {
	Namespace                  string        `json:"namespace"`
	Duration                   time.Duration `json:"duration"`
	EpisodesRead               int           `json:"episodes_read"`
	FactsCreated               int           `json:"facts_created"`
	FactsDeduplicated          int           `json:"facts_deduplicated"`
	RelationshipsFound         int           `json:"relationships_found"`
	CausalLinksFound           int           `json:"causal_links_found"`
	PatternsFound              int           `json:"patterns_found"`
	ContradictionsFound        int           `json:"contradictions_found"`
	ContradictionsAutoResolved int           `json:"contradictions_auto_resolved"`
	GoalsAnnotated             int           `json:"goals_annotated"`
	GoalsSuggestedComplete     int           `json:"goals_suggested_complete"`
	FailureRepeatsDetected     int           `json:"failure_repeats_detected"`
	FailurePatternsFound       int           `json:"failure_patterns_found"`
	HypothesesAutoConfirmed    int           `json:"hypotheses_auto_confirmed"`
	HypothesesAutoRejected     int           `json:"hypotheses_auto_rejected"`
	HypothesesUpdated          int           `json:"hypotheses_updated"`
	FactsDecayed               int           `json:"facts_decayed"`
	FactsExpired               int           `json:"facts_expired"`
	LLMCalls                   int           `json:"llm_calls"`
	Errors                     []string      `json:"errors,omitempty"`
}

// Consolidate runs the full 8-stage consolidation pipeline for a namespace.
func (b *Brain) Consolidate(ctx context.Context, namespaceSlug string) (ConsolidationResult, error) {
	if err := validatePath(namespaceSlug); err != nil {
		return ConsolidationResult{}, err
	}
	nsID, err := b.resolveNamespaceID(ctx, namespaceSlug)
	if err != nil {
		return ConsolidationResult{}, err
	}
	return b.ConsolidateByID(ctx, nsID)
}

// ConsolidateByID runs the full 8-stage consolidation pipeline for a namespace by ID.
// Stages run in execution order:
// 1. Episodes -> Facts                    (cluster + synthesize, with inline contradiction detection)
// 2. Facts -> Relationships               (extract entity edges)
// 3. Facts -> Causal Links                (cause/effect relationships)
// 4. Goal Progress Inference              (annotate goals from new facts)
// 5. Failure Pattern Detection            (recurring failures across episodes)
// 6. Facts + Relationships -> Patterns    (extract abstractions)
// 7. Hypothesis Evidence Scanning         (auto-confirm/reject pending hypotheses)
// 8. Confidence Decay                     (pure-SQL, no LLM)
func (b *Brain) ConsolidateByID(ctx context.Context, nsID int64) (ConsolidationResult, error) {
	start := time.Now()

	// A session-level PostgreSQL advisory lock prevents the background ticker
	// and a manual MCP call (or a second replica) from consolidating the same
	// namespace concurrently. Holding the acquired connection is required for
	// the lifetime of a session advisory lock.
	lockConn, err := b.pool.Acquire(ctx)
	if err != nil {
		return ConsolidationResult{}, fmt.Errorf("acquire consolidation lock connection: %w", err)
	}
	var locked bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", nsID).Scan(&locked); err != nil {
		lockConn.Release()
		return ConsolidationResult{}, fmt.Errorf("acquire consolidation advisory lock: %w", err)
	}
	if !locked {
		lockConn.Release()
		return ConsolidationResult{}, ErrConsolidationInProgress
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = lockConn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", nsID)
		lockConn.Release()
	}()

	var namespaceSlug string
	_ = b.pool.QueryRow(ctx, "SELECT slug FROM namespaces WHERE id = $1", nsID).Scan(&namespaceSlug)

	result := ConsolidationResult{Namespace: namespaceSlug}

	cp, err := b.GetOrCreateConsolidationProgress(ctx, nsID)
	if err != nil {
		return result, fmt.Errorf("get progress: %w", err)
	}

	// Every reasoner-backed stage below runs against `bc`, a shallow Brain copy
	// whose reasoner enforces a hard per-cycle call ceiling. Stages call
	// b.reasoner exactly as before, so no stage can opt out of the ceiling and
	// no stage had to be taught about it.
	bc, budget := b.withCycleBudget()

	reasonerBlocked := false
	reasonerReady := func() bool {
		if reasonerBlocked {
			return false
		}
		// A cycle that has spent its allowance stops here rather than letting
		// each remaining stage discover exhaustion one paid-looking call at a
		// time.
		if budget.Exhausted() {
			result.Errors = append(result.Errors, ErrCycleBudgetExhausted.Error())
			reasonerBlocked = true
			return false
		}
		if availability, ok := bc.reasoner.(interface{ Availability() error }); ok {
			if err := availability.Availability(); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("reasoner circuit open: %v", err))
				reasonerBlocked = true
				return false
			}
		}
		return true
	}

	// Stage 1: Episodes -> Facts (+ Stage 4: Contradiction detection)
	if ctx.Err() == nil && reasonerReady() {
		factsCreated, factsDeduped, episodesRead, llmCalls, contFound, contAuto, errs := bc.consolidateEpisodesToFacts(ctx, nsID, cp)
		result.FactsCreated = factsCreated
		result.FactsDeduplicated = factsDeduped
		result.EpisodesRead = episodesRead
		result.LLMCalls += llmCalls
		result.ContradictionsFound = contFound
		result.ContradictionsAutoResolved = contAuto
		result.Errors = append(result.Errors, errs...)
	}

	// Stage 2: Facts -> Relationships
	if ctx.Err() == nil && reasonerReady() {
		relCount, llmCalls, errs := bc.consolidateFactsToRelationships(ctx, nsID, cp)
		result.RelationshipsFound = relCount
		result.LLMCalls += llmCalls
		result.Errors = append(result.Errors, errs...)
	}

	// Stage 3.5: Facts -> Causal Links
	if ctx.Err() == nil && reasonerReady() {
		causalCount, llmCalls, errs := bc.consolidateFactsToCausalLinks(ctx, nsID, cp)
		result.CausalLinksFound = causalCount
		result.LLMCalls += llmCalls
		result.Errors = append(result.Errors, errs...)
	}

	// Stage 6: Goal Progress Inference
	if ctx.Err() == nil && reasonerReady() {
		annotated, suggestedComplete, llmCalls, errs := bc.consolidateGoalProgress(ctx, nsID, cp)
		result.GoalsAnnotated = annotated
		result.GoalsSuggestedComplete = suggestedComplete
		result.LLMCalls += llmCalls
		result.Errors = append(result.Errors, errs...)
	}

	// Stage 7: Failure Pattern Detection
	if ctx.Err() == nil && reasonerReady() {
		repeats, patterns, llmCalls, errs := bc.consolidateFailurePatterns(ctx, nsID, cp)
		result.FailureRepeatsDetected = repeats
		result.FailurePatternsFound = patterns
		result.LLMCalls += llmCalls
		result.Errors = append(result.Errors, errs...)
	}

	// Stage 3: Facts + Relationships -> Patterns
	if ctx.Err() == nil && reasonerReady() {
		patCount, llmCalls, errs := bc.consolidateToPatterns(ctx, nsID, cp)
		result.PatternsFound = patCount
		result.LLMCalls += llmCalls
		result.Errors = append(result.Errors, errs...)
	}

	// Stage 8: Hypothesis Evidence Scanning
	if ctx.Err() == nil && reasonerReady() {
		autoConfirmed, autoRejected, updated, llmCalls, errs := bc.consolidateHypothesisEvidence(ctx, nsID, cp)
		result.HypothesesAutoConfirmed = autoConfirmed
		result.HypothesesAutoRejected = autoRejected
		result.HypothesesUpdated = updated
		result.LLMCalls += llmCalls
		result.Errors = append(result.Errors, errs...)
	}

	// Stage 5: Confidence decay
	if ctx.Err() == nil {
		decayResult, err := b.DecayConfidence(ctx, nsID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("decay confidence: %v", err))
		} else {
			result.FactsDecayed = decayResult.FactsDecayed
			result.FactsExpired = decayResult.FactsExpired
		}
	}

	// Save progress
	now := time.Now().UTC()
	cp.LastRun = &now
	saveCtx := ctx
	if ctx.Err() != nil {
		saveCtx = context.Background()
	}
	if err := b.SaveConsolidationProgress(saveCtx, *cp); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("save progress: %v", err))
	}

	result.Duration = time.Since(start)
	observability.RecordConsolidation(observability.Observation{
		Namespace:          namespaceSlug,
		EventsRead:         result.EpisodesRead,
		EventsProcessed:    result.EpisodesRead,
		FactsCreated:       result.FactsCreated,
		FactsDeduplicated:  result.FactsDeduplicated,
		RelationshipsFound: result.RelationshipsFound,
		LLMCalls:           result.LLMCalls,
		Duration:           result.Duration,
		Errors:             len(result.Errors),
	})

	return result, nil
}

// --- Stage 1: Episodes -> Facts ---

func (b *Brain) consolidateEpisodesToFacts(ctx context.Context, nsID int64, cp *models.ConsolidationProgress) (created, deduped, read, llmCalls, contradictionsFound, contradictionsAutoResolved int, errs []string) {
	var outcome stageOutcome

	sql, args, err := b.queries.FetchEpisodes(nsID, cp.LastEpisodeID, b.consolidationBatchLimit(0))
	if err != nil {
		errs = append(errs, fmt.Sprintf("build fetch episodes: %v", err))
		return
	}

	rows, err := b.pool.Query(ctx, sql, args...)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch episodes: %v", err))
		return
	}
	defer rows.Close()

	var episodes []models.Episode
	for rows.Next() {
		var e models.Episode
		if err := rows.Scan(&e.ID, &e.NamespaceID, &e.Content, &e.Embedding, &e.EmbeddingModel, &e.OccurredAt, &e.CreatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan episode: %v", err))
			outcome.blocking++
			continue
		}
		episodes = append(episodes, e)
	}
	if err := rows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("episode rows: %v", err))
		return
	}

	read = len(episodes)
	if read == 0 {
		return
	}

	// Cluster by vector similarity
	clusters := b.clusterEpisodes(episodes)

	var maxID int64
	processed := make(map[int64]bool)

	for _, cluster := range clusters {
		if ctx.Err() != nil {
			break
		}

		var clusterMaxID int64
		for _, e := range cluster {
			if e.ID > maxID {
				maxID = e.ID
			}
			if e.ID > clusterMaxID {
				clusterMaxID = e.ID
			}
		}

		var texts []string
		var episodeIDs []int64
		for _, e := range cluster {
			texts = append(texts, e.Content)
			episodeIDs = append(episodeIDs, e.ID)
		}

		sf, err := b.reasoner.ReasonStructured(ctx, texts)
		llmCalls++
		if err != nil {
			errs = append(errs, fmt.Sprintf("reason structured: %v", err))
			// Quarantine is keyed on the cluster's highest episode ID: that is
			// what the watermark would have to pass to leave this cluster
			// behind. See TOL-295.
			outcome.note(ctx, b, nsID, StageEpisodesToFacts, clusterMaxID, err)
			continue
		}
		b.clearRecordQuarantine(ctx, nsID, StageEpisodesToFacts, clusterMaxID)

		if sf.Summary == "" {
			for _, e := range cluster {
				processed[e.ID] = true
			}
			continue
		}

		// Embed the fact content
		vec, err := b.embedder.Embed(ctx, sf.Summary)
		if err != nil {
			errs = append(errs, fmt.Sprintf("embed fact: %v", err))
			// Embedder/infrastructure failure, not bad content: keep the
			// watermark pinned so this cluster is reprocessed.
			outcome.blocking++
			continue
		}

		// Check for duplicate fact
		dup, err := b.factExistsByVector(ctx, nsID, vec)
		if err != nil {
			errs = append(errs, fmt.Sprintf("check duplicate: %v", err))
			outcome.blocking++
			continue
		}
		if dup {
			deduped++
			for _, e := range cluster {
				processed[e.ID] = true
			}
			continue
		}

		confidence := calculateConfidence(len(cluster), sf.Entity != "" && sf.Property != "")
		now := time.Now().UTC()

		var factID int64
		err = b.pool.QueryRow(ctx,
			`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, entity, property, value, valid_from)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
			nsID, sf.Summary, pgvector.NewVector(vec), b.embedder.Model(), confidence,
			strPtrOrNull(sf.Entity), strPtrOrNull(sf.Property), strPtrOrNull(sf.Value), now,
		).Scan(&factID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("insert fact: %v", err))
			outcome.blocking++
			continue
		}
		created++

		// Insert fact_sources
		for _, eid := range episodeIDs {
			_, _ = b.pool.Exec(ctx,
				"INSERT INTO fact_sources (fact_id, episode_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
				factID, eid,
			)
		}

		// Stage 4: Contradiction detection
		newFact := &models.Fact{
			ID:          factID,
			NamespaceID: nsID,
			Content:     sf.Summary,
			Confidence:  confidence,
			Entity:      strPtrOrNull(sf.Entity),
			Property:    strPtrOrNull(sf.Property),
			Value:       strPtrOrNull(sf.Value),
		}
		cd, ca, _ := b.DetectContradictions(ctx, nsID, newFact)
		contradictionsFound += cd
		contradictionsAutoResolved += ca

		for _, e := range cluster {
			processed[e.ID] = true
		}
	}

	// Advance unless something still genuinely needs retrying. Quarantined
	// clusters do not block: they have already had their attempts.
	if outcome.canAdvance() && maxID > cp.LastEpisodeID {
		cp.LastEpisodeID = maxID
	}
	return
}

func (b *Brain) clusterEpisodes(episodes []models.Episode) [][]models.Episode {
	if len(episodes) == 0 {
		return nil
	}

	clustered := make(map[int64]bool)
	var clusters [][]models.Episode

	for _, seed := range episodes {
		if clustered[seed.ID] {
			continue
		}

		cluster := []models.Episode{seed}
		clustered[seed.ID] = true

		if seed.Embedding.Slice() == nil {
			clusters = append(clusters, cluster)
			continue
		}

		seedVec := seed.Embedding.Slice()
		for _, candidate := range episodes {
			if clustered[candidate.ID] {
				continue
			}
			candVec := candidate.Embedding.Slice()
			if candVec == nil {
				continue
			}
			sim := cosineSimilarity(seedVec, candVec)
			if sim >= float32(b.config.SimilarityThreshold) {
				cluster = append(cluster, candidate)
				clustered[candidate.ID] = true
			}
		}

		clusters = append(clusters, cluster)
	}

	return clusters
}

func (b *Brain) factExistsByVector(ctx context.Context, nsID int64, vec []float32) (bool, error) {
	var id int64
	var score float32
	err := b.pool.QueryRow(ctx,
		`SELECT id, 1 - (embedding <=> $2) AS score FROM facts
		 WHERE namespace_id = $1 AND deleted_at IS NULL AND embedding IS NOT NULL
		 AND (valid_until IS NULL OR valid_until > now())
		 ORDER BY embedding <=> $2 LIMIT 1`,
		nsID, pgvector.NewVector(vec),
	).Scan(&id, &score)
	if err != nil {
		return false, nil
	}
	return score >= float32(b.config.DedupThreshold), nil
}

func calculateConfidence(observationCount int, hasStructuredFields bool) float32 {
	if observationCount == 0 {
		return 0.0
	}
	base := float32(observationCount) / float32(observationCount+2)
	if hasStructuredFields {
		base = base + (1-base)*0.3
	}
	if base > 1.0 {
		base = 1.0
	}
	return base
}

func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float32
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}

func strPtrOrNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// --- Stage 2: Facts -> Relationships ---

func (b *Brain) consolidateFactsToRelationships(ctx context.Context, nsID int64, cp *models.ConsolidationProgress) (count, llmCalls int, errs []string) {
	var outcome stageOutcome

	sql, args, err := b.queries.FetchFacts(nsID, cp.LastFactID, b.consolidationBatchLimit(50))
	if err != nil {
		errs = append(errs, fmt.Sprintf("build fetch facts: %v", err))
		return
	}

	rows, err := b.pool.Query(ctx, sql, args...)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch facts: %v", err))
		return
	}
	defer rows.Close()

	var facts []models.Fact
	for rows.Next() {
		var f models.Fact
		if err := rows.Scan(&f.ID, &f.NamespaceID, &f.Content, &f.Embedding, &f.EmbeddingModel, &f.Confidence, &f.Entity, &f.Property, &f.Value, &f.ValidFrom, &f.ValidUntil, &f.CreatedAt, &f.UpdatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan fact: %v", err))
			outcome.blocking++
			continue
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("fact rows: %v", err))
		return
	}

	if len(facts) == 0 {
		return
	}

	var maxID int64
	for _, fact := range facts {
		if ctx.Err() != nil {
			break
		}
		if fact.ID > maxID {
			maxID = fact.ID
		}

		rels, err := b.reasoner.ReasonRelationships(ctx, fact.Content)
		llmCalls++
		if err != nil {
			errs = append(errs, fmt.Sprintf("reason relationships fact %d: %v", fact.ID, err))
			// A transient failure keeps the watermark pinned so we retry. A
			// permanent one (unparseable output, entity not in content) counts
			// an attempt and, past the ceiling, lets the watermark move on.
			// Without this a single bad fact is re-sent to the paid reasoner
			// every cycle forever — see TOL-295.
			outcome.note(ctx, b, nsID, StageFactsToRelations, fact.ID, err)
			continue
		}
		b.clearRecordQuarantine(ctx, nsID, StageFactsToRelations, fact.ID)

		for _, rel := range rels {
			if rel.FromEntity == "" || rel.RelationType == "" || rel.ToEntity == "" {
				continue
			}

			// Check for existing relationship from this fact
			exists, _ := b.relationshipExists(ctx, nsID, rel.FromEntity, rel.RelationType, rel.ToEntity, fact.ID)
			if exists {
				continue
			}

			_, err := b.pool.Exec(ctx,
				`INSERT INTO relationships (namespace_id, from_entity, relation_type, to_entity, confidence, source_fact_id)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				nsID, rel.FromEntity, rel.RelationType, rel.ToEntity, rel.Confidence, fact.ID,
			)
			if err != nil {
				errs = append(errs, fmt.Sprintf("insert relationship: %v", err))
				// A write failure is infrastructure, not bad content: keep the
				// watermark pinned so the fact is reprocessed.
				outcome.blocking++
				continue
			}
			count++
		}
	}

	// Advance unless something still genuinely needs retrying. Quarantined
	// records do not block: they have already had their attempts.
	if outcome.canAdvance() && maxID > cp.LastFactID {
		cp.LastFactID = maxID
	}
	return
}

func (b *Brain) relationshipExists(ctx context.Context, nsID int64, from, relType, to string, sourceFactID int64) (bool, error) {
	var id int64
	err := b.pool.QueryRow(ctx,
		`SELECT id FROM relationships
		 WHERE namespace_id = $1 AND from_entity = $2 AND relation_type = $3 AND to_entity = $4
		 AND source_fact_id = $5 AND deleted_at IS NULL LIMIT 1`,
		nsID, from, relType, to, sourceFactID,
	).Scan(&id)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// --- Stage 3.5: Facts -> Causal Links ---

func (b *Brain) consolidateFactsToCausalLinks(ctx context.Context, nsID int64, cp *models.ConsolidationProgress) (count, llmCalls int, errs []string) {
	causalBatchLimit := b.consolidationBatchLimitAtLeast(30, 2)
	sql, args, err := b.queries.FetchFacts(nsID, cp.LastCausalFactID, causalBatchLimit)
	if err != nil {
		errs = append(errs, fmt.Sprintf("build fetch facts for causal: %v", err))
		return
	}

	rows, err := b.pool.Query(ctx, sql, args...)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch facts for causal: %v", err))
		return
	}
	defer rows.Close()

	var facts []models.Fact
	for rows.Next() {
		var f models.Fact
		if err := rows.Scan(&f.ID, &f.NamespaceID, &f.Content, &f.Embedding, &f.EmbeddingModel, &f.Confidence, &f.Entity, &f.Property, &f.Value, &f.ValidFrom, &f.ValidUntil, &f.CreatedAt, &f.UpdatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan fact for causal: %v", err))
			continue
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("fact rows for causal: %v", err))
		return
	}

	if len(facts) < 2 {
		return
	}

	var maxID int64
	for _, fact := range facts {
		if fact.ID > maxID {
			maxID = fact.ID
		}
	}

	llmCalls++
	found, detectErrs := b.DetectCausalLinks(ctx, nsID, facts)
	count = found
	errs = append(errs, detectErrs...)
	if len(errs) == 0 && maxID > cp.LastCausalFactID {
		cp.LastCausalFactID = maxID
	}
	return
}

// --- Stage 3: Facts + Relationships -> Patterns ---

func (b *Brain) consolidateToPatterns(ctx context.Context, nsID int64, cp *models.ConsolidationProgress) (count, llmCalls int, errs []string) {
	// Fetch new facts since last pattern extraction
	factSQL, factArgs, err := b.queries.FetchFacts(nsID, cp.LastPatternFactID, b.consolidationBatchLimit(30))
	if err != nil {
		errs = append(errs, fmt.Sprintf("build fetch facts for patterns: %v", err))
		return
	}

	factRows, err := b.pool.Query(ctx, factSQL, factArgs...)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch facts for patterns: %v", err))
		return
	}
	defer factRows.Close()

	var facts []models.Fact
	for factRows.Next() {
		var f models.Fact
		if err := factRows.Scan(&f.ID, &f.NamespaceID, &f.Content, &f.Embedding, &f.EmbeddingModel, &f.Confidence, &f.Entity, &f.Property, &f.Value, &f.ValidFrom, &f.ValidUntil, &f.CreatedAt, &f.UpdatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan fact for pattern: %v", err))
			continue
		}
		facts = append(facts, f)
	}
	if err := factRows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("fact rows for patterns: %v", err))
		return
	}

	if len(facts) == 0 {
		return
	}

	// Fetch new relationships since last pattern extraction
	relSQL, relArgs, err := b.queries.FetchRelationships(nsID, cp.LastPatternRelID, b.consolidationBatchLimit(50))
	if err != nil {
		errs = append(errs, fmt.Sprintf("build fetch rels for patterns: %v", err))
		return
	}

	relRows, err := b.pool.Query(ctx, relSQL, relArgs...)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch rels for patterns: %v", err))
		return
	}
	defer relRows.Close()

	var rels []models.Relationship
	for relRows.Next() {
		var r models.Relationship
		if err := relRows.Scan(&r.ID, &r.NamespaceID, &r.FromEntity, &r.RelationType, &r.ToEntity, &r.Confidence, &r.SourceFactID, &r.CreatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan rel for pattern: %v", err))
			continue
		}
		rels = append(rels, r)
	}
	if err := relRows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("rel rows for patterns: %v", err))
	}

	if len(rels) == 0 && len(facts) < 3 {
		// Not enough data for pattern extraction
		b.updatePatternCheckpoint(ctx, cp, facts, rels, len(errs) == 0)
		return
	}

	// Call reasoner for pattern extraction
	patterns, err := b.reasoner.ReasonPatterns(ctx, facts, rels)
	llmCalls++
	if err != nil {
		errs = append(errs, fmt.Sprintf("reason patterns: %v", err))
		// Don't update checkpoint on error
		return
	}

	for _, p := range patterns {
		if p.Content == "" {
			continue
		}

		// Confidence = min(source confidences) * coherence_score
		confidence := p.CoherenceScore
		if len(p.SourceFactIDs) > 0 || len(p.SourceRelIDs) > 0 {
			minConf := float32(1.0)
			for _, fid := range p.SourceFactIDs {
				for _, f := range facts {
					if f.ID == fid && f.Confidence < minConf {
						minConf = f.Confidence
					}
				}
			}
			for _, rid := range p.SourceRelIDs {
				for _, r := range rels {
					if r.ID == rid && r.Confidence < minConf {
						minConf = r.Confidence
					}
				}
			}
			confidence = minConf * p.CoherenceScore
		}

		// If no source IDs provided, use all facts/rels as sources
		sourceFactIDs := p.SourceFactIDs
		sourceRelIDs := p.SourceRelIDs
		if len(sourceFactIDs) == 0 || len(sourceRelIDs) == 0 {
			sourceFactIDs = make([]int64, len(facts))
			for i, f := range facts {
				sourceFactIDs[i] = f.ID
			}
			sourceRelIDs = make([]int64, len(rels))
			for i, r := range rels {
				sourceRelIDs[i] = r.ID
			}
		}

		_, err := b.pool.Exec(ctx,
			`INSERT INTO patterns (namespace_id, content, confidence, source_fact_ids, source_rel_ids, coherence_score)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			nsID, p.Content, confidence, sourceFactIDs, sourceRelIDs, p.CoherenceScore,
		)
		if err != nil {
			errs = append(errs, fmt.Sprintf("insert pattern: %v", err))
			continue
		}
		count++
	}

	// Only update checkpoint if no errors occurred (bullet-proof: prevents losing patterns)
	b.updatePatternCheckpoint(ctx, cp, facts, rels, len(errs) == 0)
	return
}

func (b *Brain) updatePatternCheckpoint(ctx context.Context, cp *models.ConsolidationProgress, facts []models.Fact, rels []models.Relationship, success bool) {
	// Only advance checkpoint if no errors occurred
	if !success {
		return
	}
	for _, f := range facts {
		if f.ID > cp.LastPatternFactID {
			cp.LastPatternFactID = f.ID
		}
	}
	for _, r := range rels {
		if r.ID > cp.LastPatternRelID {
			cp.LastPatternRelID = r.ID
		}
	}
}
