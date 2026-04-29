package brain

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/pgvector/pgvector-go"
)

// RecallResult is a unified result from semantic search across episodes and facts.
type RecallResult struct {
	ID          int64     `json:"id"`
	NamespaceID int64     `json:"namespace_id"`
	Content     string    `json:"content"`
	Confidence  float32   `json:"confidence,omitempty"`
	Score       float32   `json:"score"`
	Type        string    `json:"type"`
	OccurredAt  string    `json:"occurred_at,omitempty"`
	ValidFrom   string    `json:"valid_from,omitempty"`
	CreatedAt   string    `json:"created_at"`
}

// RecallResponse wraps recall results with persona metadata.
type RecallResponse struct {
	Persona  string            `json:"persona"`
	Personas map[string]string `json:"personas"`
	Results  []RecallResult    `json:"results"`
}

// Recall searches episodes and facts by semantic similarity across the given namespaces.
// Each namespace path matches itself and all descendants. Namespaces is required.
func (b *Brain) Recall(ctx context.Context, namespaces []string, query string, limit int) (RecallResponse, error) {
	if err := validateContent(query); err != nil {
		return RecallResponse{}, err
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	vec, err := b.embedder.Embed(ctx, query)
	if err != nil {
		return RecallResponse{}, fmt.Errorf("embed: %w", err)
	}

	pgVec := pgvector.NewVector(vec)

	nsIDs, err := b.resolveNamespaceIDs(ctx, namespaces)
	if err != nil {
		return RecallResponse{}, err
	}

	// Build personas map keyed by slug for all resolved namespaces.
	personas := make(map[string]string)
	for _, id := range nsIDs {
		ns, err := b.GetNamespaceByID(ctx, id)
		if err != nil {
			// ignore namespace retrieval errors for persona gathering
			continue
		}
		personas[ns.Slug] = ns.Persona
	}

	// Determine primary persona: prefer first non-empty persona from the
	// requested namespace list (in order), otherwise the first non-empty
	// persona from the gathered map.
	primary := ""
	for _, slug := range namespaces {
		if slug == "" {
			continue
		}
		ns, err := b.GetNamespace(ctx, slug)
		if err == nil && ns.Persona != "" {
			primary = ns.Persona
			break
		}
	}
	if primary == "" {
		for _, p := range personas {
			if p != "" {
				primary = p
				break
			}
		}
	}

	// Search facts first (higher quality, consolidated)
	factLimit := limit
	factSQL, factArgs, err := b.queries.RecallFacts(nsIDs, pgVec, factLimit)
	if err != nil {
		return RecallResponse{}, fmt.Errorf("build fact query: %w", err)
	}

	factRows, err := b.pool.Query(ctx, factSQL, factArgs...)
	if err != nil {
		return RecallResponse{}, fmt.Errorf("query facts: %w", err)
	}
	defer factRows.Close()

	var results []RecallResult
	for factRows.Next() {
		var id int64
		var namespaceID int64
		var content string
		var confidence float32
		var score float32
		var createdAt time.Time

		if err := factRows.Scan(&id, &namespaceID, &content, &confidence, &createdAt, &score); err != nil {
			return RecallResponse{}, fmt.Errorf("scan fact: %w", err)
		}
		results = append(results, RecallResult{
			ID:          id,
			NamespaceID: namespaceID,
			Content:     content,
			Confidence:  confidence,
			Score:       score,
			Type:        "fact",
			CreatedAt:   createdAt.Format(time.RFC3339),
		})
	}
	if err := factRows.Err(); err != nil {
		return RecallResponse{}, fmt.Errorf("fact rows: %w", err)
	}

	// Search episodes for remaining slots
	episodeLimit := limit - len(results)
	if episodeLimit > 0 {
		epSQL, epArgs, err := b.queries.RecallEpisodes(nsIDs, pgVec, episodeLimit)
		if err != nil {
			return RecallResponse{}, fmt.Errorf("build episode query: %w", err)
		}

		epRows, err := b.pool.Query(ctx, epSQL, epArgs...)
		if err != nil {
			return RecallResponse{}, fmt.Errorf("query episodes: %w", err)
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
				return RecallResponse{}, fmt.Errorf("scan episode: %w", err)
			}
			results = append(results, RecallResult{
				ID:          id,
				NamespaceID: namespaceID,
				Content:     content,
				Score:       score,
				Type:        "episode",
				OccurredAt:  occurredAt.Format(time.RFC3339),
				CreatedAt:   createdAt.Format(time.RFC3339),
			})
		}
		if err := epRows.Err(); err != nil {
			return RecallResponse{}, fmt.Errorf("episode rows: %w", err)
		}
	}

	// Sort all results by score descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	// Optional LLM rerank: convert RecallResult -> models.Fact and call Reasoner.Rerank
	if b.reasoner != nil {
		var candidateFacts []models.Fact
		for _, r := range results {
			candidateFacts = append(candidateFacts, models.Fact{
				ID:          r.ID,
				NamespaceID: r.NamespaceID,
				Content:     r.Content,
				Confidence:  r.Confidence,
			})
		}
		if len(candidateFacts) > 0 {
			if reranked, err := b.reasoner.Rerank(ctx, query, candidateFacts); err == nil && len(reranked) > 0 {
				// map id -> RecallResult for quick lookup
				byID := make(map[int64]RecallResult)
				for _, rr := range results {
					byID[rr.ID] = rr
				}
				var reordered []RecallResult
				for _, f := range reranked {
					if rr, ok := byID[f.ID]; ok {
						reordered = append(reordered, rr)
					}
				}
				// Append any remaining results that weren't in reranked
				seen := make(map[int64]bool)
				for _, r := range reordered {
					seen[r.ID] = true
				}
				for _, r := range results {
					if !seen[r.ID] {
						reordered = append(reordered, r)
					}
				}
				if len(reordered) > 0 {
					results = reordered
				}
			}
		}
	}

	resp := RecallResponse{
		Persona:  primary,
		Personas: personas,
		Results:  results,
	}
	return resp, nil
}
