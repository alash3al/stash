package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/jackc/pgx/v5"
)

// CreateNamespace creates a new namespace with the given slug, name, and description.
// Parent namespaces are auto-created with slug as name if they don't exist.
func (b *Brain) CreateNamespace(ctx context.Context, slug, name, description string) (int64, error) {
	if err := validatePath(slug); err != nil {
		return 0, err
	}

	segments := splitPath(slug)
	if len(segments) == 0 {
		var id int64
		err := b.pool.QueryRow(ctx,
			"INSERT INTO namespaces (slug, name) VALUES ('/', '/') ON CONFLICT (slug) DO UPDATE SET updated_at = now() RETURNING id",
		).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("create root namespace: %w", err)
		}
		return id, nil
	}

	currentPath := ""
	for i, seg := range segments {
		currentPath += "/" + seg
		if i < len(segments)-1 {
			var id int64
			_ = b.pool.QueryRow(ctx,
				"INSERT INTO namespaces (slug, name) VALUES ($1, $1) ON CONFLICT (slug) DO UPDATE SET updated_at = now() RETURNING id",
				currentPath,
			).Scan(&id)
		}
	}

	var id int64
	err := b.pool.QueryRow(ctx,
		"INSERT INTO namespaces (slug, name, description) VALUES ($1, $2, $3) ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description, updated_at = now() RETURNING id",
		slug, name, description,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create namespace: %w", err)
	}
	return id, nil
}

// GetNamespace returns a namespace by slug.
func (b *Brain) GetNamespace(ctx context.Context, slug string) (*models.Namespace, error) {
	var ns models.Namespace
	err := b.pool.QueryRow(ctx,
		"SELECT id, slug, name, description, created_at, updated_at FROM namespaces WHERE slug = $1",
		slug,
	).Scan(&ns.ID, &ns.Slug, &ns.Name, &ns.Description, &ns.CreatedAt, &ns.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNamespaceNotFound
		}
		return nil, fmt.Errorf("get namespace: %w", err)
	}
	return &ns, nil
}

// ListNamespaces returns namespaces, optionally filtered by slug paths.
// If slugs is empty, returns all namespaces.
// Each path matches itself and all descendants.
func (b *Brain) ListNamespaces(ctx context.Context, slugs []string, page Pagination) ([]models.Namespace, error) {
	page = page.Sanitize()

	if len(slugs) == 0 {
		rows, err := b.pool.Query(ctx,
			"SELECT id, slug, name, description, created_at, updated_at FROM namespaces ORDER BY slug LIMIT $1 OFFSET $2",
			page.Limit, page.Offset,
		)
		if err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}
		defer rows.Close()

		var result []models.Namespace
		for rows.Next() {
			var ns models.Namespace
			if err := rows.Scan(&ns.ID, &ns.Slug, &ns.Name, &ns.Description, &ns.CreatedAt, &ns.UpdatedAt); err != nil {
				return nil, fmt.Errorf("scan namespace: %w", err)
			}
			result = append(result, ns)
		}
		return result, rows.Err()
	}

	ids, err := b.resolveNamespaceIDs(ctx, slugs)
	if err != nil {
		return nil, err
	}

	rows, err := b.pool.Query(ctx,
		"SELECT id, slug, name, description, created_at, updated_at FROM namespaces WHERE id = ANY($1) ORDER BY slug LIMIT $2 OFFSET $3",
		ids, page.Limit, page.Offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer rows.Close()

	var result []models.Namespace
	for rows.Next() {
		var ns models.Namespace
		if err := rows.Scan(&ns.ID, &ns.Slug, &ns.Name, &ns.Description, &ns.CreatedAt, &ns.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan namespace: %w", err)
		}
		result = append(result, ns)
	}
	return result, rows.Err()
}

// GetOrCreateConsolidationProgress returns progress for a namespace, creating a row if needed.
func (b *Brain) GetOrCreateConsolidationProgress(ctx context.Context, namespaceID int64) (*models.ConsolidationProgress, error) {
	var cp models.ConsolidationProgress
	err := b.pool.QueryRow(ctx,
		`INSERT INTO consolidation_progress (namespace_id) VALUES ($1)
		 ON CONFLICT (namespace_id) DO UPDATE SET updated_at = consolidation_progress.updated_at
		 RETURNING namespace_id, last_episode_id, last_fact_id, last_relationship_id, last_pattern_fact_id, last_pattern_rel_id, last_goal_progress_fact_id, last_failure_id, last_failure_episode_id, last_hypothesis_fact_id, last_causal_fact_id, last_decay_run, last_run, updated_at`,
		namespaceID,
	).Scan(&cp.NamespaceID, &cp.LastEpisodeID, &cp.LastFactID, &cp.LastRelationshipID, &cp.LastPatternFactID, &cp.LastPatternRelID, &cp.LastGoalProgressFactID, &cp.LastFailureID, &cp.LastFailureEpisodeID, &cp.LastHypothesisFactID, &cp.LastCausalFactID, &cp.LastDecayRun, &cp.LastRun, &cp.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get consolidation progress: %w", err)
	}
	return &cp, nil
}

// SaveConsolidationProgress updates the checkpoint for a namespace.
func (b *Brain) SaveConsolidationProgress(ctx context.Context, cp models.ConsolidationProgress) error {
	now := time.Now().UTC()
	_, err := b.pool.Exec(ctx,
		`UPDATE consolidation_progress SET
			last_episode_id = $2, last_fact_id = $3, last_relationship_id = $4,
			last_pattern_fact_id = $5, last_pattern_rel_id = $6,
			last_goal_progress_fact_id = $7, last_failure_id = $8, last_failure_episode_id = $9, last_hypothesis_fact_id = $10,
			last_causal_fact_id = $11, last_decay_run = $12, last_run = $13, updated_at = $14
		 WHERE namespace_id = $1`,
		cp.NamespaceID, cp.LastEpisodeID, cp.LastFactID, cp.LastRelationshipID,
		cp.LastPatternFactID, cp.LastPatternRelID,
		cp.LastGoalProgressFactID, cp.LastFailureID, cp.LastFailureEpisodeID, cp.LastHypothesisFactID,
		cp.LastCausalFactID, cp.LastDecayRun, now, now,
	)
	if err != nil {
		return fmt.Errorf("save consolidation progress: %w", err)
	}
	return nil
}
