package brain

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/models"
	"github.com/alash3al/stash/internal/reasoner"
)

func (b *Brain) consolidateHypothesisEvidence(ctx context.Context, nsID int64, cp *models.ConsolidationProgress) (autoConfirmed, autoRejected, updated, llmCalls int, errs []string) {
	rows, err := b.pool.Query(ctx,
		`SELECT id, namespace_id, content, confidence, status, verification_plan, method,
		 confirmed_fact_id, rejection_reason, source_fact_ids, tested_at, confirmed_at, rejected_at,
		 created_at, updated_at, deleted_at
		 FROM hypotheses WHERE namespace_id = $1 AND status IN ('proposed', 'testing') AND deleted_at IS NULL`,
		nsID,
	)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch open hypotheses: %v", err))
		return
	}
	defer rows.Close()

	hypotheses, err := scanHypothesisRows(rows)
	if err != nil {
		errs = append(errs, fmt.Sprintf("scan hypotheses: %v", err))
		return
	}

	if len(hypotheses) == 0 {
		return
	}

	factSQL, factArgs, err := b.queries.FetchFacts(nsID, cp.LastHypothesisFactID, b.consolidationBatchLimit(30))
	if err != nil {
		errs = append(errs, fmt.Sprintf("build fetch facts for hypotheses: %v", err))
		return
	}

	factRows, err := b.pool.Query(ctx, factSQL, factArgs...)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch facts for hypotheses: %v", err))
		return
	}
	defer factRows.Close()

	var facts []models.Fact
	for factRows.Next() {
		var f models.Fact
		if err := factRows.Scan(&f.ID, &f.NamespaceID, &f.Content, &f.Embedding, &f.EmbeddingModel, &f.Confidence, &f.Entity, &f.Property, &f.Value, &f.ValidFrom, &f.ValidUntil, &f.CreatedAt, &f.UpdatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan fact for hypotheses: %v", err))
			continue
		}
		facts = append(facts, f)
	}
	if err := factRows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("fact rows for hypotheses: %v", err))
		return
	}

	if len(facts) == 0 {
		return
	}

	var results []*reasoner.HypothesisEvidenceResult
	hypothesisBatchLimit := b.consolidationBatchLimit(30)
	for start := 0; start < len(hypotheses); start += hypothesisBatchLimit {
		end := start + hypothesisBatchLimit
		if end > len(hypotheses) {
			end = len(hypotheses)
		}
		llmCalls++
		batchResults, reasonErr := b.reasoner.ReasonHypothesisEvidence(ctx, hypotheses[start:end], facts)
		if reasonErr != nil {
			errs = append(errs, fmt.Sprintf("reason hypothesis evidence: %v", reasonErr))
			return
		}
		results = append(results, batchResults...)
	}

	for _, r := range results {
		hyp, getErr := b.GetHypothesis(ctx, r.HypothesisID)
		if getErr != nil {
			continue
		}

		switch {
		case r.Verdict == "supports" && r.Confidence >= b.config.HypothesisAutoConfirmThreshold:
			switch hyp.Status {
			case "proposed":
				_, statusErr := b.UpdateHypothesisStatus(ctx, r.HypothesisID, "testing")
				if statusErr != nil {
					errs = append(errs, fmt.Sprintf("auto-transition hypothesis %d to testing: %v", r.HypothesisID, statusErr))
					continue
				}
				updated++
			case "testing":
				_, _, confirmErr := b.ConfirmHypothesis(ctx, r.HypothesisID)
				if confirmErr != nil {
					errs = append(errs, fmt.Sprintf("auto-confirm hypothesis %d: %v", r.HypothesisID, confirmErr))
					continue
				}
				autoConfirmed++
			}

		case r.Verdict == "contradicts" && r.Confidence >= b.config.HypothesisAutoRejectThreshold:
			reason := r.Reasoning
			if reason == "" {
				reason = "Auto-rejected: contradicting evidence detected during consolidation"
			}
			_, rejectErr := b.RejectHypothesis(ctx, r.HypothesisID, reason)
			if rejectErr != nil {
				errs = append(errs, fmt.Sprintf("auto-reject hypothesis %d: %v", r.HypothesisID, rejectErr))
				continue
			}
			autoRejected++

		case r.Verdict == "supports" || r.Verdict == "weakens":
			_, err := b.pool.Exec(ctx,
				`UPDATE hypotheses SET confidence = $2, updated_at = now() WHERE id = $1`,
				r.HypothesisID, r.NewConfidence,
			)
			if err != nil {
				errs = append(errs, fmt.Sprintf("update hypothesis %d confidence: %v", r.HypothesisID, err))
				continue
			}
			updated++
		}
	}

	var maxFactID int64
	for _, f := range facts {
		if f.ID > maxFactID {
			maxFactID = f.ID
		}
	}
	if len(errs) == 0 && maxFactID > cp.LastHypothesisFactID {
		cp.LastHypothesisFactID = maxFactID
	}

	return
}
