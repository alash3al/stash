package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/jackc/pgx/v5"
)

var (
	ErrGoalNotFound  = fmt.Errorf("brain: goal not found")
	ErrGoalNotActive = fmt.Errorf("brain: goal is not active")
)

const goalColumns = `id, namespace_id, parent_id, content, status, priority, notes,
completed_at, abandoned_at, created_at, updated_at, deleted_at`

func scanGoal(h *models.Goal, row pgx.Row) error {
	return row.Scan(
		&h.ID, &h.NamespaceID, &h.ParentID, &h.Content, &h.Status, &h.Priority, &h.Notes,
		&h.CompletedAt, &h.AbandonedAt, &h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
	)
}

func scanGoalRows(rows pgx.Rows) ([]models.Goal, error) {
	var result []models.Goal
	for rows.Next() {
		var g models.Goal
		if err := rows.Scan(
			&g.ID, &g.NamespaceID, &g.ParentID, &g.Content, &g.Status, &g.Priority, &g.Notes,
			&g.CompletedAt, &g.AbandonedAt, &g.CreatedAt, &g.UpdatedAt, &g.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan goal: %w", err)
		}
		result = append(result, g)
	}
	return result, rows.Err()
}

// CreateGoal creates a new goal in active status.
func (b *Brain) CreateGoal(ctx context.Context, nsID int64, content string, parentID *int64, priority int) (*models.Goal, error) {
	if content == "" {
		return nil, ErrEmptyContent
	}

	if parentID != nil {
		parent, err := b.GetGoal(ctx, *parentID)
		if err != nil {
			return nil, fmt.Errorf("check parent goal: %w", err)
		}
		if parent.Status != "active" {
			return nil, fmt.Errorf("%w: parent goal %d is %s, must be active", ErrGoalNotActive, *parentID, parent.Status)
		}
	}

	var g models.Goal
	err := b.pool.QueryRow(ctx,
		`INSERT INTO goals (namespace_id, parent_id, content, priority)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+goalColumns,
		nsID, parentID, content, priority,
	).Scan(
		&g.ID, &g.NamespaceID, &g.ParentID, &g.Content, &g.Status, &g.Priority, &g.Notes,
		&g.CompletedAt, &g.AbandonedAt, &g.CreatedAt, &g.UpdatedAt, &g.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create goal: %w", err)
	}
	return &g, nil
}

// ListGoals returns goals across namespaces, optionally filtered by status and parent.
func (b *Brain) ListGoals(ctx context.Context, namespaceSlugs []string, status string, parentID *int64, page Pagination) ([]models.Goal, error) {
	nsIDs, err := b.resolveNamespaceIDs(ctx, namespaceSlugs)
	if err != nil {
		return nil, err
	}

	page = page.Sanitize()

	query := `SELECT ` + goalColumns + ` FROM goals WHERE namespace_id = ANY($1) AND deleted_at IS NULL`
	args := []any{nsIDs}
	argN := 1

	if status != "" {
		argN++
		query += fmt.Sprintf(" AND status = $%d", argN)
		args = append(args, status)
	}

	if parentID != nil {
		argN++
		query += fmt.Sprintf(" AND parent_id = $%d", argN)
		args = append(args, *parentID)
	} else if status == "" {
		query += " AND parent_id IS NULL"
	}

	argN++
	query += fmt.Sprintf(" ORDER BY priority DESC, created_at ASC LIMIT $%d", argN)
	args = append(args, page.Limit)

	argN++
	query += fmt.Sprintf(" OFFSET $%d", argN)
	args = append(args, page.Offset)

	rows, err := b.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list goals: %w", err)
	}
	defer rows.Close()
	return scanGoalRows(rows)
}

// GetGoal returns a single goal by ID.
func (b *Brain) GetGoal(ctx context.Context, id int64) (*models.Goal, error) {
	var g models.Goal
	err := b.pool.QueryRow(ctx,
		`SELECT `+goalColumns+` FROM goals WHERE id = $1`, id,
	).Scan(
		&g.ID, &g.NamespaceID, &g.ParentID, &g.Content, &g.Status, &g.Priority, &g.Notes,
		&g.CompletedAt, &g.AbandonedAt, &g.CreatedAt, &g.UpdatedAt, &g.DeletedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrGoalNotFound
		}
		return nil, fmt.Errorf("get goal: %w", err)
	}
	return &g, nil
}

// GetGoalProgress returns sub-goal counts for a parent goal.
func (b *Brain) GetGoalProgress(ctx context.Context, id int64) (total, completed int, err error) {
	err = b.pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE status IN ('active', 'completed')),
		        COUNT(*) FILTER (WHERE status = 'completed')
		 FROM goals WHERE parent_id = $1 AND deleted_at IS NULL`,
		id,
	).Scan(&total, &completed)
	if err != nil {
		return 0, 0, fmt.Errorf("get goal progress: %w", err)
	}
	return total, completed, nil
}

// CompleteGoal marks a goal as completed. If all siblings are completed, auto-completes the parent.
func (b *Brain) CompleteGoal(ctx context.Context, id int64, notes string) (*models.Goal, error) {
	current, err := b.GetGoal(ctx, id)
	if err != nil {
		return nil, err
	}

	if current.Status != "active" {
		return nil, fmt.Errorf("%w: goal %d is %s, must be active", ErrGoalNotActive, id, current.Status)
	}

	now := time.Now().UTC()

	var g models.Goal
	err = b.pool.QueryRow(ctx,
		`UPDATE goals SET status = 'completed', completed_at = $2, notes = CASE WHEN $3 = '' THEN notes ELSE $3 END, updated_at = $2
		 WHERE id = $1
		 RETURNING `+goalColumns,
		id, now, notes,
	).Scan(
		&g.ID, &g.NamespaceID, &g.ParentID, &g.Content, &g.Status, &g.Priority, &g.Notes,
		&g.CompletedAt, &g.AbandonedAt, &g.CreatedAt, &g.UpdatedAt, &g.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("complete goal: %w", err)
	}

	if g.ParentID != nil {
		if err := b.autoCompleteParent(ctx, *g.ParentID); err != nil {
			return &g, nil
		}
	}

	return &g, nil
}

func (b *Brain) autoCompleteParent(ctx context.Context, parentID int64) error {
	var total, completed int
	err := b.pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE status IN ('active', 'completed')),
		        COUNT(*) FILTER (WHERE status = 'completed')
		 FROM goals WHERE parent_id = $1 AND deleted_at IS NULL`,
		parentID,
	).Scan(&total, &completed)
	if err != nil {
		return err
	}

	if total > 0 && total == completed {
		now := time.Now().UTC()
		_, err := b.pool.Exec(ctx,
			`UPDATE goals SET status = 'completed', completed_at = $2, updated_at = $2 WHERE id = $1 AND status = 'active'`,
			parentID, now,
		)
		if err != nil {
			return err
		}

		var grandparentID *int64
		err = b.pool.QueryRow(ctx,
			"SELECT parent_id FROM goals WHERE id = $1", parentID,
		).Scan(&grandparentID)
		if err != nil {
			return err
		}
		if grandparentID != nil {
			return b.autoCompleteParent(ctx, *grandparentID)
		}
	}

	return nil
}

// AbandonGoal marks a goal as abandoned.
func (b *Brain) AbandonGoal(ctx context.Context, id int64, notes string) (*models.Goal, error) {
	current, err := b.GetGoal(ctx, id)
	if err != nil {
		return nil, err
	}

	if current.Status != "active" {
		return nil, fmt.Errorf("%w: goal %d is %s, must be active", ErrGoalNotActive, id, current.Status)
	}

	now := time.Now().UTC()

	var g models.Goal
	err = b.pool.QueryRow(ctx,
		`UPDATE goals SET status = 'abandoned', abandoned_at = $2, notes = CASE WHEN $3 = '' THEN notes ELSE $3 END, updated_at = $2
		 WHERE id = $1
		 RETURNING `+goalColumns,
		id, now, notes,
	).Scan(
		&g.ID, &g.NamespaceID, &g.ParentID, &g.Content, &g.Status, &g.Priority, &g.Notes,
		&g.CompletedAt, &g.AbandonedAt, &g.CreatedAt, &g.UpdatedAt, &g.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("abandon goal: %w", err)
	}
	return &g, nil
}

// UpdateGoal updates content, priority, and notes of an active goal.
func (b *Brain) UpdateGoal(ctx context.Context, id int64, content string, priority int, notes string) (*models.Goal, error) {
	current, err := b.GetGoal(ctx, id)
	if err != nil {
		return nil, err
	}

	if current.Status != "active" {
		return nil, fmt.Errorf("%w: goal %d is %s, must be active", ErrGoalNotActive, id, current.Status)
	}

	if content == "" {
		content = current.Content
	}
	if priority == 0 {
		priority = current.Priority
	}
	if notes == "" {
		notes = current.Notes
	}

	now := time.Now().UTC()

	var g models.Goal
	err = b.pool.QueryRow(ctx,
		`UPDATE goals SET content = $2, priority = $3, notes = $4, updated_at = $5
		 WHERE id = $1
		 RETURNING `+goalColumns,
		id, content, priority, notes, now,
	).Scan(
		&g.ID, &g.NamespaceID, &g.ParentID, &g.Content, &g.Status, &g.Priority, &g.Notes,
		&g.CompletedAt, &g.AbandonedAt, &g.CreatedAt, &g.UpdatedAt, &g.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("update goal: %w", err)
	}
	return &g, nil
}

// DeleteGoal soft-deletes a goal by ID. Children cascade via FK.
func (b *Brain) DeleteGoal(ctx context.Context, id int64) error {
	tag, err := b.pool.Exec(ctx,
		"UPDATE goals SET deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL",
		id,
	)
	if err != nil {
		return fmt.Errorf("delete goal: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrGoalNotFound
	}
	return nil
}
