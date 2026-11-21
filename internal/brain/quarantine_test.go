package brain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/alash3al/stash/internal/reasoner"
)

// The regression this file exists for (TOL-295): before the fix, ANY error kept
// a stage's watermark pinned, so one permanently-unparseable record was re-sent
// to the paid reasoner every five minutes forever. Production logged ~2,300
// such wasted calls a day.

func TestTransientFailuresBlockTheWatermark(t *testing.T) {
	transient := []struct {
		name string
		err  error
	}{
		{"provider circuit open", &reasoner.UnavailableError{StatusCode: 429, RetryAt: time.Now().Add(time.Minute)}},
		{"cycle budget exhausted", ErrCycleBudgetExhausted},
		{"wrapped cycle budget", fmt.Errorf("stage failed: %w", ErrCycleBudgetExhausted)},
		{"context canceled", context.Canceled},
		{"context deadline", context.DeadlineExceeded},
	}
	for _, test := range transient {
		t.Run(test.name, func(t *testing.T) {
			if !isTransientReasonerError(test.err) {
				t.Fatalf("%v classified permanent; a recoverable failure must be retried, not quarantined", test.err)
			}
			// Attempt counts must be irrelevant: a transient error can never
			// exhaust a record's attempts, no matter how often it recurs.
			if !shouldBlockWatermark(test.err, 99, 3, true) {
				t.Fatal("transient failure must pin the watermark regardless of attempts")
			}
		})
	}
}

func TestPermanentFailureIsQuarantinedOnlyAfterItsAttempts(t *testing.T) {
	// The real production errors from TOL-295.
	poison := errors.New("parse json: unexpected end of JSON input")

	for attempts := 1; attempts < 3; attempts++ {
		if !shouldBlockWatermark(poison, attempts, 3, true) {
			t.Fatalf("attempt %d/3: watermark advanced early; a flaky response must get its retries", attempts)
		}
	}
	if shouldBlockWatermark(poison, 3, 3, true) {
		t.Fatal("attempt 3/3: watermark still pinned — this is the infinite paid retry loop (TOL-295)")
	}
	if shouldBlockWatermark(poison, 4, 3, true) {
		t.Fatal("past the ceiling the record must stay quarantined")
	}
}

func TestBookkeepingFailureFailsClosed(t *testing.T) {
	// If the quarantine UPSERT itself fails we cannot know the attempt count.
	// Retrying costs money; skipping loses data. Choose the recoverable one.
	if !shouldBlockWatermark(errors.New("permanent"), 0, 3, false) {
		t.Fatal("a failed quarantine write must pin the watermark, never skip the record")
	}
}

func TestNilErrorNeverBlocks(t *testing.T) {
	if isTransientReasonerError(nil) {
		t.Fatal("nil is not a failure")
	}
}

/* ── Per-cycle reasoner ceiling ──────────────────────────────────────────── */

// countingReasoner records how many calls actually reached the provider.
type countingReasoner struct{ calls int }

func (c *countingReasoner) ReasonStructured(context.Context, []string) (*reasoner.StructuredFact, error) {
	c.calls++
	return &reasoner.StructuredFact{}, nil
}
func (c *countingReasoner) ReasonRelationships(context.Context, string) ([]*reasoner.StructuredRelationship, error) {
	c.calls++
	return nil, nil
}
func (c *countingReasoner) ReasonPatterns(context.Context, []models.Fact, []models.Relationship) ([]*reasoner.StructuredPattern, error) {
	c.calls++
	return nil, nil
}
func (c *countingReasoner) ReasonContradiction(context.Context, string, string, string, string) (*reasoner.ContradictionResult, error) {
	c.calls++
	return nil, nil
}
func (c *countingReasoner) ReasonCausalLinks(context.Context, []models.Fact) ([]*reasoner.StructuredCausalLink, error) {
	c.calls++
	return nil, nil
}
func (c *countingReasoner) ReasonGoalProgress(context.Context, []models.Goal, []models.Fact) ([]*reasoner.GoalProgressAssessment, error) {
	c.calls++
	return nil, nil
}
func (c *countingReasoner) ReasonFailurePatterns(context.Context, []models.Failure, []string) ([]*reasoner.FailurePatternResult, error) {
	c.calls++
	return nil, nil
}
func (c *countingReasoner) ReasonHypothesisEvidence(context.Context, []models.Hypothesis, []models.Fact) ([]*reasoner.HypothesisEvidenceResult, error) {
	c.calls++
	return nil, nil
}

func TestCycleCeilingStopsSpendRegardlessOfWatermarkState(t *testing.T) {
	inner := &countingReasoner{}
	budget := newBudgetedReasoner(inner, 3)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := budget.ReasonRelationships(ctx, "fact"); err != nil {
			t.Fatalf("call %d was inside the ceiling but failed: %v", i+1, err)
		}
	}
	if inner.calls != 3 {
		t.Fatalf("provider saw %d calls, want 3", inner.calls)
	}

	// The 4th must be refused locally — it must never reach the provider.
	if _, err := budget.ReasonRelationships(ctx, "fact"); !errors.Is(err, ErrCycleBudgetExhausted) {
		t.Fatalf("call past the ceiling returned %v, want ErrCycleBudgetExhausted", err)
	}
	if inner.calls != 3 {
		t.Fatalf("provider saw %d calls after exhaustion; the ceiling leaked spend", inner.calls)
	}
	if !budget.Exhausted() {
		t.Fatal("budget should report exhausted")
	}
	if budget.Used() != 3 {
		t.Fatalf("Used() = %d, want 3", budget.Used())
	}
}

func TestCeilingIsSharedAcrossEveryStage(t *testing.T) {
	// A ceiling that each stage could spend separately is not a ceiling.
	inner := &countingReasoner{}
	budget := newBudgetedReasoner(inner, 2)
	ctx := context.Background()

	if _, err := budget.ReasonStructured(ctx, []string{"a"}); err != nil {
		t.Fatalf("stage 1: %v", err)
	}
	if _, err := budget.ReasonCausalLinks(ctx, nil); err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	if _, err := budget.ReasonPatterns(ctx, nil, nil); !errors.Is(err, ErrCycleBudgetExhausted) {
		t.Fatalf("stage 3 returned %v, want the shared ceiling to refuse it", err)
	}
	if inner.calls != 2 {
		t.Fatalf("provider saw %d calls, want 2", inner.calls)
	}
}

func TestExhaustedCeilingIsTransientSoNothingGetsQuarantined(t *testing.T) {
	// Records skipped because the cycle ran out of budget were never judged.
	// They must not burn quarantine attempts, or a busy cycle would silently
	// quarantine healthy records.
	inner := &countingReasoner{}
	budget := newBudgetedReasoner(inner, 0)
	_, err := budget.ReasonRelationships(context.Background(), "fact")
	if !isTransientReasonerError(err) {
		t.Fatal("ceiling exhaustion must be transient, never a quarantine attempt")
	}
}

func TestStageOutcomeAdvancesOnlyWhenNothingIsPending(t *testing.T) {
	var clean stageOutcome
	if !clean.canAdvance() {
		t.Fatal("a stage with no blocking failures must advance its watermark")
	}
	var blocked stageOutcome
	blocked.blocking++
	if blocked.canAdvance() {
		t.Fatal("a stage with a pending retry must not advance its watermark")
	}
}

func TestConfiguredLimitsOverrideDefaults(t *testing.T) {
	def := &Brain{}
	if def.maxRecordAttempts() != DefaultMaxRecordAttempts {
		t.Fatalf("default attempts = %d, want %d", def.maxRecordAttempts(), DefaultMaxRecordAttempts)
	}
	if def.cycleReasonerCallCeiling() != DefaultCycleReasonerCallCeiling {
		t.Fatalf("default ceiling = %d, want %d", def.cycleReasonerCallCeiling(), DefaultCycleReasonerCallCeiling)
	}

	tuned := &Brain{config: Config{MaxRecordAttempts: 7, CycleReasonerCallCeiling: 11}}
	if tuned.maxRecordAttempts() != 7 {
		t.Fatalf("configured attempts = %d, want 7", tuned.maxRecordAttempts())
	}
	if tuned.cycleReasonerCallCeiling() != 11 {
		t.Fatalf("configured ceiling = %d, want 11", tuned.cycleReasonerCallCeiling())
	}
}

func TestPersistedErrorIsBounded(t *testing.T) {
	// Provider errors can embed an entire response body; the column is read by
	// operators, not machines.
	long := errors.New(string(make([]byte, 5000)))
	if got := len(truncateError(long)); got > 520 {
		t.Fatalf("persisted error length %d, want bounded", got)
	}
	if truncateError(nil) != "" {
		t.Fatal("nil error must persist as empty")
	}
}
