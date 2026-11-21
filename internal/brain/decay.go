package brain

import (
	"context"
	"fmt"
)

// DecayResult describes the outcome of a confidence decay run.
type DecayResult struct {
	FactsDecayed int `json:"facts_decayed"`
	FactsExpired int `json:"facts_expired"`
}

// DecayConfidence reduces confidence of facts not re-observed within the configured window.
// Facts below the expiry threshold are soft-deleted. This is pure SQL — no LLM calls.
func (b *Brain) DecayConfidence(ctx context.Context, nsID int64) (DecayResult, error) {
	if b.config.DecayFactor <= 0 || b.config.DecayFactor >= 1 {
		return DecayResult{}, nil
	}

	var result DecayResult

	decayTag, err := b.pool.Exec(ctx,
		`UPDATE facts SET confidence = confidence * $3, updated_at = now()
		 WHERE namespace_id = $1 AND deleted_at IS NULL AND valid_until IS NULL
		 AND updated_at < now() - $2::interval`,
		nsID, b.config.Window.String(), b.config.DecayFactor,
	)
	if err != nil {
		return result, fmt.Errorf("decay confidence: %w", err)
	}
	result.FactsDecayed = int(decayTag.RowsAffected())

	expireTag, err := b.pool.Exec(ctx,
		`UPDATE facts SET valid_until = now(), updated_at = now()
		 WHERE namespace_id = $1 AND deleted_at IS NULL AND valid_until IS NULL
		 AND confidence < $2`,
		nsID, b.config.ExpiryThreshold,
	)
	if err != nil {
		return result, fmt.Errorf("expire low-confidence facts: %w", err)
	}
	result.FactsExpired = int(expireTag.RowsAffected())

	return result, nil
}
