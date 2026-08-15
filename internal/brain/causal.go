package brain

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/models"
	"github.com/jackc/pgx/v5"
)

var ErrCausalLinkNotFound = fmt.Errorf("brain: causal link not found")

// DetectCausalLinks feeds a batch of facts to the reasoner and inserts extracted causal links.
func (b *Brain) DetectCausalLinks(ctx context.Context, nsID int64, facts []models.Fact) (int, []string) {
	if len(facts) < 2 {
		return 0, nil
	}

	links, err := b.reasoner.ReasonCausalLinks(ctx, facts)
	if err != nil {
		return 0, []string{fmt.Sprintf("reason causal links: %v", err)}
	}

	var count int
	for _, link := range links {
		if link.CauseFactID == link.EffectFactID {
			continue
		}

		tag, err := b.pool.Exec(ctx,
			`INSERT INTO causal_links (namespace_id, cause_fact_id, effect_fact_id, confidence, method)
			 VALUES ($1, $2, $3, $4, 'extracted')
			 ON CONFLICT (cause_fact_id, effect_fact_id) WHERE deleted_at IS NULL DO NOTHING`,
			nsID, link.CauseFactID, link.EffectFactID, link.Confidence,
		)
		if err != nil {
			return count, []string{fmt.Sprintf("insert causal link: %v", err)}
		}
		count += int(tag.RowsAffected())
	}

	return count, nil
}

// ListCausalLinks returns causal links for namespaces matching the given paths.
func (b *Brain) ListCausalLinks(ctx context.Context, namespaceSlugs []string, page Pagination) ([]models.CausalLink, error) {
	nsIDs, err := b.resolveNamespaceIDs(ctx, namespaceSlugs)
	if err != nil {
		return nil, err
	}

	page = page.Sanitize()

	rows, err := b.pool.Query(ctx,
		`SELECT id, namespace_id, cause_fact_id, effect_fact_id, confidence, method, created_at, deleted_at
		 FROM causal_links WHERE namespace_id = ANY($1) AND deleted_at IS NULL
		 ORDER BY id LIMIT $2 OFFSET $3`,
		nsIDs, page.Limit, page.Offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list causal links: %w", err)
	}
	defer rows.Close()

	var result []models.CausalLink
	for rows.Next() {
		var cl models.CausalLink
		if err := rows.Scan(&cl.ID, &cl.NamespaceID, &cl.CauseFactID, &cl.EffectFactID, &cl.Confidence, &cl.Method, &cl.CreatedAt, &cl.DeletedAt); err != nil {
			return nil, fmt.Errorf("scan causal link: %w", err)
		}
		result = append(result, cl)
	}
	return result, rows.Err()
}

// CreateCausalLink manually asserts a cause-effect relationship between two facts.
func (b *Brain) CreateCausalLink(ctx context.Context, nsID, causeFactID, effectFactID int64, confidence float32) (*models.CausalLink, error) {
	if causeFactID == effectFactID {
		return nil, fmt.Errorf("brain: cause and effect fact IDs must differ")
	}

	var cl models.CausalLink
	err := b.pool.QueryRow(ctx,
		`INSERT INTO causal_links (namespace_id, cause_fact_id, effect_fact_id, confidence, method)
		 VALUES ($1, $2, $3, $4, 'asserted')
		 ON CONFLICT (cause_fact_id, effect_fact_id) WHERE deleted_at IS NULL DO NOTHING
		 RETURNING id, namespace_id, cause_fact_id, effect_fact_id, confidence, method, created_at`,
		nsID, causeFactID, effectFactID, confidence,
	).Scan(&cl.ID, &cl.NamespaceID, &cl.CauseFactID, &cl.EffectFactID, &cl.Confidence, &cl.Method, &cl.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("brain: causal link already exists between facts %d and %d", causeFactID, effectFactID)
		}
		return nil, fmt.Errorf("create causal link: %w", err)
	}
	return &cl, nil
}

// DeleteCausalLink soft-deletes a causal link by ID.
func (b *Brain) DeleteCausalLink(ctx context.Context, id int64) error {
	tag, err := b.pool.Exec(ctx,
		"UPDATE causal_links SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL",
		id,
	)
	if err != nil {
		return fmt.Errorf("delete causal link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCausalLinkNotFound
	}
	return nil
}

// TraceCausalChain returns the causal chain starting from a fact, using a bounded recursive CTE.
// direction: "forward" (what did this fact cause?) or "backward" (what caused this fact?).
func (b *Brain) TraceCausalChain(ctx context.Context, factID int64, direction string, maxDepth int) ([]models.CausalLink, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}

	var anchorCol, joinCol string
	switch direction {
	case "backward":
		anchorCol = "effect_fact_id"
		joinCol = "cause_fact_id"
	default:
		anchorCol = "cause_fact_id"
		joinCol = "effect_fact_id"
	}

	query := fmt.Sprintf(`
		WITH RECURSIVE chain AS (
			SELECT id, namespace_id, cause_fact_id, effect_fact_id, confidence, method, created_at, 1 AS depth
			FROM causal_links
			WHERE %s = $1 AND deleted_at IS NULL
			UNION ALL
			SELECT cl.id, cl.namespace_id, cl.cause_fact_id, cl.effect_fact_id, cl.confidence, cl.method, cl.created_at, c.depth + 1
			FROM causal_links cl JOIN chain c ON cl.%s = c.%s
			WHERE cl.deleted_at IS NULL AND c.depth < $2
		)
		SELECT id, namespace_id, cause_fact_id, effect_fact_id, confidence, method, created_at
		FROM chain ORDER BY depth`,
		anchorCol, anchorCol, joinCol,
	)

	rows, err := b.pool.Query(ctx, query, factID, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("trace causal chain: %w", err)
	}
	defer rows.Close()

	var result []models.CausalLink
	for rows.Next() {
		var cl models.CausalLink
		if err := rows.Scan(&cl.ID, &cl.NamespaceID, &cl.CauseFactID, &cl.EffectFactID, &cl.Confidence, &cl.Method, &cl.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan causal chain: %w", err)
		}
		result = append(result, cl)
	}
	return result, rows.Err()
}
