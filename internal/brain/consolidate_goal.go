package brain

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/models"
	"github.com/alash3al/stash/internal/reasoner"
)

func (b *Brain) consolidateGoalProgress(ctx context.Context, nsID int64, cp *models.ConsolidationProgress) (annotated, suggestedComplete, llmCalls int, errs []string) {
	rows, err := b.pool.Query(ctx,
		`SELECT id, namespace_id, parent_id, content, status, priority, notes,
		 completed_at, abandoned_at, created_at, updated_at, deleted_at
		 FROM goals WHERE namespace_id = $1 AND status = 'active' AND deleted_at IS NULL`,
		nsID,
	)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch active goals: %v", err))
		return
	}
	defer rows.Close()

	goals, err := scanGoalRows(rows)
	if err != nil {
		errs = append(errs, fmt.Sprintf("scan goals: %v", err))
		return
	}

	if len(goals) == 0 {
		return
	}

	factSQL, factArgs, err := b.queries.FetchFacts(nsID, cp.LastGoalProgressFactID, b.consolidationBatchLimit(30))
	if err != nil {
		errs = append(errs, fmt.Sprintf("build fetch facts for goals: %v", err))
		return
	}

	factRows, err := b.pool.Query(ctx, factSQL, factArgs...)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch facts for goals: %v", err))
		return
	}
	defer factRows.Close()

	var facts []models.Fact
	for factRows.Next() {
		var f models.Fact
		if err := factRows.Scan(&f.ID, &f.NamespaceID, &f.Content, &f.Embedding, &f.EmbeddingModel, &f.Confidence, &f.Entity, &f.Property, &f.Value, &f.ValidFrom, &f.ValidUntil, &f.CreatedAt, &f.UpdatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan fact for goals: %v", err))
			continue
		}
		facts = append(facts, f)
	}
	if err := factRows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("fact rows for goals: %v", err))
		return
	}

	if len(facts) == 0 {
		return
	}

	var assessments []*reasoner.GoalProgressAssessment
	goalBatchLimit := b.consolidationBatchLimit(30)
	for start := 0; start < len(goals); start += goalBatchLimit {
		end := start + goalBatchLimit
		if end > len(goals) {
			end = len(goals)
		}
		llmCalls++
		batchAssessments, reasonErr := b.reasoner.ReasonGoalProgress(ctx, goals[start:end], facts)
		if reasonErr != nil {
			errs = append(errs, fmt.Sprintf("reason goal progress: %v", reasonErr))
			return
		}
		assessments = append(assessments, batchAssessments...)
	}

	for _, a := range assessments {
		var note string
		switch a.Assessment {
		case "progress":
			note = fmt.Sprintf("\n[PROGRESS] %s (confidence: %.2f)", a.Note, a.Confidence)
			annotated++
		case "suggested_complete":
			note = fmt.Sprintf("\n[SUGGESTED COMPLETE] %s (confidence: %.2f)", a.Note, a.Confidence)
			annotated++
			suggestedComplete++
		case "contradicted":
			note = fmt.Sprintf("\n[CONTRADICTED] %s (confidence: %.2f)", a.Note, a.Confidence)
			annotated++
		default:
			continue
		}

		_, err := b.pool.Exec(ctx,
			`UPDATE goals SET notes = notes || $2, updated_at = now() WHERE id = $1`,
			a.GoalID, note,
		)
		if err != nil {
			errs = append(errs, fmt.Sprintf("annotate goal %d: %v", a.GoalID, err))
		}
	}

	var maxFactID int64
	for _, f := range facts {
		if f.ID > maxFactID {
			maxFactID = f.ID
		}
	}
	if len(errs) == 0 && maxFactID > cp.LastGoalProgressFactID {
		cp.LastGoalProgressFactID = maxFactID
	}

	return
}
