package brain

import (
	"context"
	"errors"
	"sync"

	"github.com/alash3al/stash/internal/models"
	"github.com/alash3al/stash/internal/reasoner"
)

// Consolidation stage names used as quarantine keys. These are persisted, so
// treat them as a stable wire format: rename one and you orphan its rows.
const (
	StageEpisodesToFacts    = "episodes_to_facts"
	StageFactsToRelations   = "facts_to_relationships"
	StageFactsToCausalLinks = "facts_to_causal_links"
	StagePatterns           = "patterns"
	StageGoalProgress       = "goal_progress"
	StageHypothesisEvidence = "hypothesis_evidence"
)

// DefaultMaxRecordAttempts is how many times a single record may fail a stage
// for a permanent reason before the watermark is allowed past it.
//
// It is deliberately greater than 1: a truncated JSON response can be
// non-deterministic, so one bad response should not permanently skip a record.
// It is deliberately small: the whole point is that retrying is bounded.
const DefaultMaxRecordAttempts = 3

// DefaultCycleReasonerCallCeiling caps reasoner calls per namespace per cycle,
// independent of watermark state.
//
// The quarantine logic below removes the known unbounded loop. This ceiling is
// the backstop for the loop we have not found yet — the lesson of the August
// 2026 Workers AI incident is that a cost path without a hard ceiling
// eventually finds a way to run away, and that discovering it from the invoice
// is too late.
const DefaultCycleReasonerCallCeiling = 50

// ErrCycleBudgetExhausted is returned once a cycle has spent its reasoner call
// allowance. It is classified TRANSIENT: the records were never really
// attempted, so they must not accrue quarantine attempts, and the watermark
// must not advance past them.
var ErrCycleBudgetExhausted = errors.New("consolidation: per-cycle reasoner call ceiling reached")

// isTransientReasonerError reports whether an error means "try again later"
// rather than "this record cannot be processed".
//
// This distinction is the entire fix. Treating every failure as transient (the
// previous behaviour) turns one unparseable record into an unbounded paid retry
// loop. Treating every failure as permanent would silently drop real data the
// first time a provider returns 503. Only the errors below are recoverable by
// waiting; everything else is a property of the record's own content.
func isTransientReasonerError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrCycleBudgetExhausted) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Circuit-open / 429 / 5xx from the provider.
	return reasoner.IsUnavailable(err)
}

// shouldBlockWatermark decides, for a single failed record, whether the stage's
// watermark must stay pinned so the record is retried.
//
// This is the whole fix expressed as one pure function, kept separate from its
// persistence so the decision can be tested exhaustively without a database.
//
//   - transient error            → block; waiting can fix this
//   - bookkeeping failed (ok=false) → block; fail closed, never lose a record
//     because an UPSERT failed
//   - permanent, under ceiling   → block; give it its remaining attempts
//   - permanent, at/over ceiling → do NOT block; quarantined, move past it
func shouldBlockWatermark(err error, attempts, maxAttempts int, ok bool) bool {
	if isTransientReasonerError(err) {
		return true
	}
	if !ok {
		return true
	}
	return attempts < maxAttempts
}

// recordFailureAttempt persists a permanent per-record stage failure and returns
// the running attempt count. ok is false when the bookkeeping write itself
// failed, which callers must treat as "keep retrying".
func (b *Brain) recordFailureAttempt(ctx context.Context, nsID int64, stage string, recordID int64, cause error) (attempts int, ok bool) {
	err := b.pool.QueryRow(ctx,
		`INSERT INTO consolidation_quarantine (namespace_id, stage, record_id, attempts, last_error)
		 VALUES ($1, $2, $3, 1, $4)
		 ON CONFLICT (namespace_id, stage, record_id) DO UPDATE
		   SET attempts   = consolidation_quarantine.attempts + 1,
		       last_error = EXCLUDED.last_error,
		       updated_at = now()
		 RETURNING attempts`,
		nsID, stage, recordID, truncateError(cause),
	).Scan(&attempts)
	if err != nil {
		return 0, false
	}
	return attempts, true
}

// clearRecordQuarantine drops the failure history for a record that has now
// succeeded, so an intermittent problem cannot accumulate across weeks into a
// spurious quarantine.
func (b *Brain) clearRecordQuarantine(ctx context.Context, nsID int64, stage string, recordID int64) {
	_, _ = b.pool.Exec(ctx,
		`DELETE FROM consolidation_quarantine
		  WHERE namespace_id = $1 AND stage = $2 AND record_id = $3`,
		nsID, stage, recordID,
	)
}

func (b *Brain) maxRecordAttempts() int {
	if b.config.MaxRecordAttempts > 0 {
		return b.config.MaxRecordAttempts
	}
	return DefaultMaxRecordAttempts
}

func (b *Brain) cycleReasonerCallCeiling() int {
	if b.config.CycleReasonerCallCeiling > 0 {
		return b.config.CycleReasonerCallCeiling
	}
	return DefaultCycleReasonerCallCeiling
}

// truncateError bounds what we persist. Provider errors can embed an entire
// response body, and this column is read by operators, not machines.
func truncateError(err error) string {
	if err == nil {
		return ""
	}
	const max = 500
	s := err.Error()
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// stageOutcome accumulates, for one stage, whether anything still needs to be
// retried. Only a TRANSIENT failure (or a permanent one still under the attempt
// ceiling) blocks the watermark. A quarantined record does not.
type stageOutcome struct {
	blocking int
}

// note classifies err for recordID and reports whether the caller should keep
// going. It is the single place the retry/quarantine decision is made, so no
// stage can accidentally reintroduce the unbounded loop.
func (o *stageOutcome) note(ctx context.Context, b *Brain, nsID int64, stage string, recordID int64, err error) {
	// A transient failure never reaches the quarantine table: the record was
	// never really judged, so it must not burn an attempt.
	if isTransientReasonerError(err) {
		o.blocking++
		return
	}
	attempts, ok := b.recordFailureAttempt(ctx, nsID, stage, recordID, err)
	if shouldBlockWatermark(err, attempts, b.maxRecordAttempts(), ok) {
		o.blocking++
	}
}

// canAdvance reports whether the stage may move its watermark to maxID.
func (o *stageOutcome) canAdvance() bool { return o.blocking == 0 }

/* ── Per-cycle reasoner call ceiling ─────────────────────────────────────── */

// budgetedReasoner wraps a Reasoner with a hard per-cycle call ceiling.
//
// It is installed by shallow-copying Brain for the duration of one cycle, so
// every existing stage keeps calling b.reasoner unchanged and none of them can
// opt out of the ceiling.
type budgetedReasoner struct {
	inner     reasoner.Reasoner
	mu        sync.Mutex
	remaining int
	used      int
}

func newBudgetedReasoner(inner reasoner.Reasoner, ceiling int) *budgetedReasoner {
	return &budgetedReasoner{inner: inner, remaining: ceiling}
}

func (r *budgetedReasoner) take() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.remaining <= 0 {
		return ErrCycleBudgetExhausted
	}
	r.remaining--
	r.used++
	return nil
}

// Used reports how many calls this cycle actually spent, for logging.
func (r *budgetedReasoner) Used() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.used
}

// Exhausted reports whether the ceiling stopped work this cycle.
func (r *budgetedReasoner) Exhausted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remaining <= 0
}

// Availability forwards the circuit-breaker probe when the inner reasoner
// supports it, so reasonerReady() keeps working through the wrapper.
func (r *budgetedReasoner) Availability() error {
	if a, ok := r.inner.(interface{ Availability() error }); ok {
		return a.Availability()
	}
	return nil
}

func (r *budgetedReasoner) ReasonStructured(ctx context.Context, texts []string) (*reasoner.StructuredFact, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.inner.ReasonStructured(ctx, texts)
}

func (r *budgetedReasoner) ReasonRelationships(ctx context.Context, factContent string) ([]*reasoner.StructuredRelationship, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.inner.ReasonRelationships(ctx, factContent)
}

func (r *budgetedReasoner) ReasonPatterns(ctx context.Context, facts []models.Fact, relationships []models.Relationship) ([]*reasoner.StructuredPattern, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.inner.ReasonPatterns(ctx, facts, relationships)
}

func (r *budgetedReasoner) ReasonContradiction(ctx context.Context, entity, property, oldValue, newValue string) (*reasoner.ContradictionResult, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.inner.ReasonContradiction(ctx, entity, property, oldValue, newValue)
}

func (r *budgetedReasoner) ReasonCausalLinks(ctx context.Context, facts []models.Fact) ([]*reasoner.StructuredCausalLink, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.inner.ReasonCausalLinks(ctx, facts)
}

func (r *budgetedReasoner) ReasonGoalProgress(ctx context.Context, goals []models.Goal, facts []models.Fact) ([]*reasoner.GoalProgressAssessment, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.inner.ReasonGoalProgress(ctx, goals, facts)
}

func (r *budgetedReasoner) ReasonFailurePatterns(ctx context.Context, failures []models.Failure, evidence []string) ([]*reasoner.FailurePatternResult, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.inner.ReasonFailurePatterns(ctx, failures, evidence)
}

func (r *budgetedReasoner) ReasonHypothesisEvidence(ctx context.Context, hypotheses []models.Hypothesis, facts []models.Fact) ([]*reasoner.HypothesisEvidenceResult, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.inner.ReasonHypothesisEvidence(ctx, hypotheses, facts)
}

// compile-time proof the wrapper stays a drop-in for the full interface.
var _ reasoner.Reasoner = (*budgetedReasoner)(nil)

// withCycleBudget returns a shallow Brain copy whose reasoner enforces a
// per-cycle call ceiling, plus the budget for reporting.
func (b *Brain) withCycleBudget() (*Brain, *budgetedReasoner) {
	budget := newBudgetedReasoner(b.reasoner, b.cycleReasonerCallCeiling())
	scoped := *b
	scoped.reasoner = budget
	return &scoped, budget
}
