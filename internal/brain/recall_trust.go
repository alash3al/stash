package brain

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pgvector/pgvector-go"
)

const recallEmbeddingDedupeThreshold float32 = 0.92

// DefaultRecallMinScore is the default STASH_RECALL_MIN_SCORE.
//
// Chosen against gold-retrieval-v1 (30 cases, /tools/baton): the lowest semantic
// score among returned gold hits at k=10 on the live corpus is ~0.18, while
// nonsense queries ("zxqwlk nonexistent topic") peak at ~0.014 (foundation audit
// 2026-11-17, stash.md §4.1). 0.05 sits a full order of magnitude above noise
// with margin under the gold floor. Set STASH_RECALL_MIN_SCORE=0 to disable.
const DefaultRecallMinScore = 0.05

// RecallResponse is the outward-facing recall payload, including trust metadata.
type RecallResponse struct {
	Results             []RecallResult `json:"results"`
	DuplicatesCollapsed int            `json:"duplicates_collapsed"`
	// BelowThreshold is true when every candidate was dropped by the score floor.
	BelowThreshold bool `json:"below_threshold,omitempty"`
}

func applyRecallMinScore(results []RecallResult, minScore float64) ([]RecallResult, bool) {
	if minScore <= 0 || len(results) == 0 {
		return results, false
	}
	filtered := results[:0]
	for _, r := range results {
		if float64(r.Score) >= minScore {
			filtered = append(filtered, r)
		}
	}
	allDropped := len(filtered) == 0 && len(results) > 0
	return filtered, allDropped
}

func recallResultTime(r RecallResult) time.Time {
	for _, raw := range []string{r.ValidFrom, r.OccurredAt, r.CreatedAt} {
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func dedupeRecallByEmbedding(results []RecallResult, embeddings map[memoryRef][]float32) ([]RecallResult, int) {
	if len(results) < 2 {
		return results, 0
	}
	removed := make([]bool, len(results))
	collapsed := 0
	for i := 0; i < len(results); i++ {
		if removed[i] {
			continue
		}
		refI := memoryRef{Type: results[i].Type, ID: results[i].ID}
		vecI := embeddings[refI]
		if len(vecI) == 0 {
			continue
		}
		for j := i + 1; j < len(results); j++ {
			if removed[j] {
				continue
			}
			refJ := memoryRef{Type: results[j].Type, ID: results[j].ID}
			vecJ := embeddings[refJ]
			if len(vecJ) == 0 {
				continue
			}
			if cosineSimilarity(vecI, vecJ) < recallEmbeddingDedupeThreshold {
				continue
			}
			ti := recallResultTime(results[i])
			tj := recallResultTime(results[j])
			if tj.After(ti) {
				removed[i] = true
				collapsed++
				break
			}
			removed[j] = true
			collapsed++
		}
	}
	out := make([]RecallResult, 0, len(results))
	for i, r := range results {
		if !removed[i] {
			out = append(out, r)
		}
	}
	return out, collapsed
}

type factRecallMeta struct {
	ValidFrom *time.Time
	CreatedAt time.Time
	Entity    string
	Property  string
}

func (b *Brain) loadRecallEmbeddings(ctx context.Context, results []RecallResult) (map[memoryRef][]float32, error) {
	var factIDs, episodeIDs []int64
	for _, r := range results {
		switch r.Type {
		case "fact":
			factIDs = append(factIDs, r.ID)
		case "episode":
			episodeIDs = append(episodeIDs, r.ID)
		}
	}
	out := make(map[memoryRef][]float32)
	if len(factIDs) > 0 {
		rows, err := b.pool.Query(ctx, `SELECT id, embedding FROM facts WHERE id = ANY($1) AND embedding IS NOT NULL`, factIDs)
		if err != nil {
			return nil, fmt.Errorf("load fact embeddings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var vec pgvector.Vector
			if err := rows.Scan(&id, &vec); err != nil {
				return nil, fmt.Errorf("scan fact embedding: %w", err)
			}
			out[memoryRef{Type: "fact", ID: id}] = vec.Slice()
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(episodeIDs) > 0 {
		rows, err := b.pool.Query(ctx, `SELECT id, embedding FROM episodes WHERE id = ANY($1) AND embedding IS NOT NULL`, episodeIDs)
		if err != nil {
			return nil, fmt.Errorf("load episode embeddings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var vec pgvector.Vector
			if err := rows.Scan(&id, &vec); err != nil {
				return nil, fmt.Errorf("scan episode embedding: %w", err)
			}
			out[memoryRef{Type: "episode", ID: id}] = vec.Slice()
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (b *Brain) loadFactRecallMeta(ctx context.Context, factIDs []int64) (map[int64]factRecallMeta, error) {
	if len(factIDs) == 0 {
		return map[int64]factRecallMeta{}, nil
	}
	rows, err := b.pool.Query(ctx,
		`SELECT id, COALESCE(entity, ''), COALESCE(property, ''), valid_from, created_at FROM facts WHERE id = ANY($1)`,
		factIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("load fact recall meta: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]factRecallMeta, len(factIDs))
	for rows.Next() {
		var id int64
		var meta factRecallMeta
		if err := rows.Scan(&id, &meta.Entity, &meta.Property, &meta.ValidFrom, &meta.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan fact recall meta: %w", err)
		}
		out[id] = meta
	}
	return out, rows.Err()
}

type supersessionPair struct {
	OlderID int64
	NewerID int64
}

func factEffectiveTime(meta factRecallMeta) time.Time {
	if meta.ValidFrom != nil {
		return meta.ValidFrom.UTC()
	}
	return meta.CreatedAt.UTC()
}

func formatFactTime(meta factRecallMeta) string {
	if meta.ValidFrom != nil {
		return meta.ValidFrom.UTC().Format(time.RFC3339)
	}
	return meta.CreatedAt.UTC().Format(time.RFC3339)
}

// contradictionRow is one (old_fact_id, new_fact_id) row from contradictions.
type contradictionRow struct {
	OldID int64
	NewID int64
}

func (b *Brain) findSupersessionPairs(ctx context.Context, factIDs []int64, meta map[int64]factRecallMeta) ([]supersessionPair, error) {
	if len(factIDs) < 2 {
		return nil, nil
	}
	rows, err := b.pool.Query(ctx,
		`SELECT old_fact_id, new_fact_id FROM contradictions
		 WHERE old_fact_id = ANY($1) AND new_fact_id = ANY($1)`,
		factIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("load contradiction pairs: %w", err)
	}
	defer rows.Close()
	var contradictions []contradictionRow
	for rows.Next() {
		var row contradictionRow
		if err := rows.Scan(&row.OldID, &row.NewID); err != nil {
			return nil, fmt.Errorf("scan contradiction pair: %w", err)
		}
		contradictions = append(contradictions, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buildSupersessionPairs(factIDs, contradictions, meta), nil
}

// buildSupersessionPairs decides which kept facts are "about the same subject":
// an explicit contradiction row, or the same non-empty entity+property. The
// older/newer side is always decided by effective time (valid_from, falling
// back to created_at), never by the contradiction row's column order. Pure so
// the decision logic is testable without a database.
func buildSupersessionPairs(factIDs []int64, contradictions []contradictionRow, meta map[int64]factRecallMeta) []supersessionPair {
	seen := map[[2]int64]bool{}
	var pairs []supersessionPair
	addPair := func(a, b int64) {
		ta, tb := factEffectiveTime(meta[a]), factEffectiveTime(meta[b])
		var older, newer int64
		switch {
		case ta.After(tb):
			older, newer = b, a
		case tb.After(ta):
			older, newer = a, b
		case a < b:
			older, newer = a, b
		default:
			older, newer = b, a
		}
		key := [2]int64{older, newer}
		if seen[key] {
			return
		}
		seen[key] = true
		pairs = append(pairs, supersessionPair{OlderID: older, NewerID: newer})
	}
	for _, c := range contradictions {
		addPair(c.OldID, c.NewID)
	}
	// Same entity+property in the result set implies the same subject.
	for i := 0; i < len(factIDs); i++ {
		for j := i + 1; j < len(factIDs); j++ {
			a, b := factIDs[i], factIDs[j]
			ma, mb := meta[a], meta[b]
			if ma.Entity == "" || ma.Property == "" {
				continue
			}
			if ma.Entity == mb.Entity && ma.Property == mb.Property {
				addPair(a, b)
			}
		}
	}
	return pairs
}

func applySupersessionRanking(results []RecallResult, pairs []supersessionPair, meta map[int64]factRecallMeta) {
	if len(pairs) == 0 {
		return
	}
	indexOf := make(map[int64]int)
	for i, r := range results {
		if r.Type == "fact" {
			indexOf[r.ID] = i
		}
	}
	for _, pair := range pairs {
		oi, okOld := indexOf[pair.OlderID]
		ni, okNew := indexOf[pair.NewerID]
		if !okOld || !okNew {
			continue
		}
		oldLabel := formatFactTime(meta[pair.OlderID])
		newLabel := formatFactTime(meta[pair.NewerID])
		dates := []string{oldLabel, newLabel}
		sort.Strings(dates)
		results[oi].SupersessionDates = dates
		results[ni].SupersessionDates = dates
		if oi < ni {
			results[oi], results[ni] = results[ni], results[oi]
			indexOf[pair.OlderID] = ni
			indexOf[pair.NewerID] = oi
		}
	}
}

// applyRecallTrust runs floor, dedupe and supersession. applyFloor is false for
// the lexical fallback: ts_rank_cd scores are not on the cosine scale the
// floor is calibrated for, so a provider outage must not silently empty recall.
func (b *Brain) applyRecallTrust(ctx context.Context, results []RecallResult, applyFloor bool) (RecallResponse, error) {
	resp := RecallResponse{Results: results}
	if len(results) == 0 {
		return resp, nil
	}

	if applyFloor {
		resp.Results, resp.BelowThreshold = applyRecallMinScore(resp.Results, b.config.RecallMinScore)
	}
	if len(resp.Results) == 0 {
		return resp, nil
	}

	embeddings, err := b.loadRecallEmbeddings(ctx, resp.Results)
	if err != nil {
		return RecallResponse{}, err
	}
	resp.Results, resp.DuplicatesCollapsed = dedupeRecallByEmbedding(resp.Results, embeddings)
	if len(resp.Results) == 0 {
		return resp, nil
	}

	var factIDs []int64
	for _, r := range resp.Results {
		if r.Type == "fact" {
			factIDs = append(factIDs, r.ID)
		}
	}
	if len(factIDs) >= 2 {
		meta, err := b.loadFactRecallMeta(ctx, factIDs)
		if err != nil {
			return RecallResponse{}, err
		}
		pairs, err := b.findSupersessionPairs(ctx, factIDs, meta)
		if err != nil {
			return RecallResponse{}, err
		}
		applySupersessionRanking(resp.Results, pairs, meta)
	}
	return resp, nil
}
