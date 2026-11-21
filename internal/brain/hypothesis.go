package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

var (
	ErrHypothesisNotFound          = fmt.Errorf("brain: hypothesis not found")
	ErrInvalidHypothesisTransition = fmt.Errorf("brain: invalid hypothesis status transition")
	ErrHypothesisAlreadyConfirmed  = fmt.Errorf("brain: hypothesis already confirmed")
)

var validTransitions = map[string][]string{
	"proposed":  {"testing", "rejected"},
	"testing":   {"confirmed", "rejected", "proposed"},
	"confirmed": {},
	"rejected":  {},
}

func isValidTransition(from, to string) bool {
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

func scanHypothesis(h *models.Hypothesis, row pgx.Row) error {
	return row.Scan(
		&h.ID, &h.NamespaceID, &h.Content, &h.Confidence, &h.Status,
		&h.VerificationPlan, &h.Method, &h.ConfirmedFactID, &h.RejectionReason,
		&h.SourceFactIDs, &h.TestedAt, &h.ConfirmedAt, &h.RejectedAt,
		&h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
	)
}

func scanHypothesisRows(rows pgx.Rows) ([]models.Hypothesis, error) {
	var result []models.Hypothesis
	for rows.Next() {
		var h models.Hypothesis
		if err := rows.Scan(
			&h.ID, &h.NamespaceID, &h.Content, &h.Confidence, &h.Status,
			&h.VerificationPlan, &h.Method, &h.ConfirmedFactID, &h.RejectionReason,
			&h.SourceFactIDs, &h.TestedAt, &h.ConfirmedAt, &h.RejectedAt,
			&h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan hypothesis: %w", err)
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// CreateHypothesis creates a new hypothesis in proposed status.
func (b *Brain) CreateHypothesis(ctx context.Context, nsID int64, content, verificationPlan string, confidence float32, sourceFactIDs []int64) (*models.Hypothesis, error) {
	if content == "" {
		return nil, ErrEmptyContent
	}
	if sourceFactIDs == nil {
		sourceFactIDs = []int64{}
	}

	var h models.Hypothesis
	err := b.pool.QueryRow(ctx,
		`INSERT INTO hypotheses (namespace_id, content, confidence, verification_plan, source_fact_ids)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, namespace_id, content, confidence, status, verification_plan, method,
		 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
		 created_at, updated_at, deleted_at`,
		nsID, content, confidence, verificationPlan, sourceFactIDs,
	).Scan(
		&h.ID, &h.NamespaceID, &h.Content, &h.Confidence, &h.Status,
		&h.VerificationPlan, &h.Method, &h.ConfirmedFactID, &h.RejectionReason,
		&h.SourceFactIDs, &h.TestedAt, &h.ConfirmedAt, &h.RejectedAt,
		&h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create hypothesis: %w", err)
	}
	return &h, nil
}

// ListHypotheses returns hypotheses across namespaces, optionally filtered by status.
func (b *Brain) ListHypotheses(ctx context.Context, namespaceSlugs []string, status string, page Pagination) ([]models.Hypothesis, error) {
	nsIDs, err := b.resolveNamespaceIDs(ctx, namespaceSlugs)
	if err != nil {
		return nil, err
	}

	page = page.Sanitize()

	if status != "" {
		rows, err := b.pool.Query(ctx,
			`SELECT id, namespace_id, content, confidence, status, verification_plan, method,
			 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
			 created_at, updated_at, deleted_at
			 FROM hypotheses WHERE namespace_id = ANY($1) AND status = $2 AND deleted_at IS NULL
			 ORDER BY updated_at DESC LIMIT $3 OFFSET $4`,
			nsIDs, status, page.Limit, page.Offset,
		)
		if err != nil {
			return nil, fmt.Errorf("list hypotheses: %w", err)
		}
		defer rows.Close()
		return scanHypothesisRows(rows)
	}

	rows, err := b.pool.Query(ctx,
		`SELECT id, namespace_id, content, confidence, status, verification_plan, method,
		 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
		 created_at, updated_at, deleted_at
		 FROM hypotheses WHERE namespace_id = ANY($1) AND deleted_at IS NULL
		 ORDER BY updated_at DESC LIMIT $2 OFFSET $3`,
		nsIDs, page.Limit, page.Offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list hypotheses: %w", err)
	}
	defer rows.Close()
	return scanHypothesisRows(rows)
}

// GetHypothesis returns a single hypothesis by ID.
func (b *Brain) GetHypothesis(ctx context.Context, id int64) (*models.Hypothesis, error) {
	var h models.Hypothesis
	err := b.pool.QueryRow(ctx,
		`SELECT id, namespace_id, content, confidence, status, verification_plan, method,
		 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
		 created_at, updated_at, deleted_at
		 FROM hypotheses WHERE id = $1`,
		id,
	).Scan(
		&h.ID, &h.NamespaceID, &h.Content, &h.Confidence, &h.Status,
		&h.VerificationPlan, &h.Method, &h.ConfirmedFactID, &h.RejectionReason,
		&h.SourceFactIDs, &h.TestedAt, &h.ConfirmedAt, &h.RejectedAt,
		&h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrHypothesisNotFound
		}
		return nil, fmt.Errorf("get hypothesis: %w", err)
	}
	return &h, nil
}

// UpdateHypothesisStatus transitions a hypothesis to a new status.
func (b *Brain) UpdateHypothesisStatus(ctx context.Context, id int64, status string) (*models.Hypothesis, error) {
	current, err := b.GetHypothesis(ctx, id)
	if err != nil {
		return nil, err
	}

	if !isValidTransition(current.Status, status) {
		return nil, fmt.Errorf("%w: %s → %s", ErrInvalidHypothesisTransition, current.Status, status)
	}

	now := time.Now().UTC()
	var testedAt, confirmedAt, rejectedAt *time.Time
	if current.TestedAt != nil {
		testedAt = current.TestedAt
	}
	if current.ConfirmedAt != nil {
		confirmedAt = current.ConfirmedAt
	}
	if current.RejectedAt != nil {
		rejectedAt = current.RejectedAt
	}

	switch status {
	case "testing":
		testedAt = &now
	case "confirmed":
		confirmedAt = &now
	case "rejected":
		rejectedAt = &now
	}

	var h models.Hypothesis
	err = b.pool.QueryRow(ctx,
		`UPDATE hypotheses SET status = $2, tested_at = $3, confirmed_at = $4, rejected_at = $5, updated_at = $6
		 WHERE id = $1
		 RETURNING id, namespace_id, content, confidence, status, verification_plan, method,
		 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
		 created_at, updated_at, deleted_at`,
		id, status, testedAt, confirmedAt, rejectedAt, now,
	).Scan(
		&h.ID, &h.NamespaceID, &h.Content, &h.Confidence, &h.Status,
		&h.VerificationPlan, &h.Method, &h.ConfirmedFactID, &h.RejectionReason,
		&h.SourceFactIDs, &h.TestedAt, &h.ConfirmedAt, &h.RejectedAt,
		&h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("update hypothesis status: %w", err)
	}
	return &h, nil
}

// ConfirmHypothesis confirms a hypothesis and auto-creates a Fact from its content.
func (b *Brain) ConfirmHypothesis(ctx context.Context, id int64) (*models.Hypothesis, *models.Fact, error) {
	current, err := b.GetHypothesis(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	if current.ConfirmedFactID != nil {
		return nil, nil, ErrHypothesisAlreadyConfirmed
	}

	if !isValidTransition(current.Status, "confirmed") {
		return nil, nil, fmt.Errorf("%w: %s → confirmed", ErrInvalidHypothesisTransition, current.Status)
	}

	vec, err := b.embedder.Embed(ctx, current.Content)
	if err != nil {
		return nil, nil, fmt.Errorf("embed confirmed hypothesis: %w", err)
	}

	now := time.Now().UTC()

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin confirm transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var factID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, valid_from)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		current.NamespaceID, current.Content, pgvector.NewVector(vec), b.embedder.Model(), current.Confidence, now,
	).Scan(&factID)
	if err != nil {
		return nil, nil, fmt.Errorf("insert fact from hypothesis: %w", err)
	}

	var h models.Hypothesis
	err = tx.QueryRow(ctx,
		`UPDATE hypotheses SET status = 'confirmed', confirmed_at = $2, confirmed_fact_id = $3, updated_at = $2
		 WHERE id = $1
		 RETURNING id, namespace_id, content, confidence, status, verification_plan, method,
		 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
		 created_at, updated_at, deleted_at`,
		id, now, factID,
	).Scan(
		&h.ID, &h.NamespaceID, &h.Content, &h.Confidence, &h.Status,
		&h.VerificationPlan, &h.Method, &h.ConfirmedFactID, &h.RejectionReason,
		&h.SourceFactIDs, &h.TestedAt, &h.ConfirmedAt, &h.RejectedAt,
		&h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update hypothesis confirmed: %w", err)
	}

	var f models.Fact
	err = tx.QueryRow(ctx,
		`SELECT id, namespace_id, content, embedding, embedding_model, confidence,
		 entity, property, value, valid_from, valid_until, created_at, updated_at, deleted_at
		 FROM facts WHERE id = $1`,
		factID,
	).Scan(
		&f.ID, &f.NamespaceID, &f.Content, &f.Embedding, &f.EmbeddingModel,
		&f.Confidence, &f.Entity, &f.Property, &f.Value,
		&f.ValidFrom, &f.ValidUntil, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("scan confirmed fact: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit confirm: %w", err)
	}

	return &h, &f, nil
}

// RejectHypothesis rejects a hypothesis with a reason.
func (b *Brain) RejectHypothesis(ctx context.Context, id int64, reason string) (*models.Hypothesis, error) {
	current, err := b.GetHypothesis(ctx, id)
	if err != nil {
		return nil, err
	}

	if !isValidTransition(current.Status, "rejected") {
		return nil, fmt.Errorf("%w: %s → rejected", ErrInvalidHypothesisTransition, current.Status)
	}

	now := time.Now().UTC()
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}

	var h models.Hypothesis
	err = b.pool.QueryRow(ctx,
		`UPDATE hypotheses SET status = 'rejected', rejected_at = $2, rejection_reason = $3, updated_at = $2
		 WHERE id = $1
		 RETURNING id, namespace_id, content, confidence, status, verification_plan, method,
		 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
		 created_at, updated_at, deleted_at`,
		id, now, reasonPtr,
	).Scan(
		&h.ID, &h.NamespaceID, &h.Content, &h.Confidence, &h.Status,
		&h.VerificationPlan, &h.Method, &h.ConfirmedFactID, &h.RejectionReason,
		&h.SourceFactIDs, &h.TestedAt, &h.ConfirmedAt, &h.RejectedAt,
		&h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("reject hypothesis: %w", err)
	}
	return &h, nil
}

// RefineHypothesis updates content/plan/confidence and resets status to proposed.
// Only allowed from testing status.
func (b *Brain) RefineHypothesis(ctx context.Context, id int64, content, verificationPlan string, confidence float32) (*models.Hypothesis, error) {
	current, err := b.GetHypothesis(ctx, id)
	if err != nil {
		return nil, err
	}

	if !isValidTransition(current.Status, "proposed") {
		return nil, fmt.Errorf("%w: can only refine from testing status, current: %s", ErrInvalidHypothesisTransition, current.Status)
	}

	if content == "" {
		content = current.Content
	}
	if verificationPlan == "" {
		verificationPlan = current.VerificationPlan
	}
	if confidence == 0 {
		confidence = current.Confidence
	}

	now := time.Now().UTC
	var h models.Hypothesis
	err = b.pool.QueryRow(ctx,
		`UPDATE hypotheses SET content = $2, verification_plan = $3, confidence = $4,
		 status = 'proposed', tested_at = NULL, updated_at = $5
		 WHERE id = $1
		 RETURNING id, namespace_id, content, confidence, status, verification_plan, method,
		 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
		 created_at, updated_at, deleted_at`,
		id, content, verificationPlan, confidence, now,
	).Scan(
		&h.ID, &h.NamespaceID, &h.Content, &h.Confidence, &h.Status,
		&h.VerificationPlan, &h.Method, &h.ConfirmedFactID, &h.RejectionReason,
		&h.SourceFactIDs, &h.TestedAt, &h.ConfirmedAt, &h.RejectedAt,
		&h.CreatedAt, &h.UpdatedAt, &h.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("refine hypothesis: %w", err)
	}
	return &h, nil
}

// DeleteHypothesis soft-deletes a hypothesis by ID.
func (b *Brain) DeleteHypothesis(ctx context.Context, id int64) error {
	tag, err := b.pool.Exec(ctx,
		"UPDATE hypotheses SET deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL",
		id,
	)
	if err != nil {
		return fmt.Errorf("delete hypothesis: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrHypothesisNotFound
	}
	return nil
}
