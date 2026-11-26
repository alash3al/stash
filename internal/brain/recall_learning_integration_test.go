package brain_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alash3al/stash/internal/brain"
	"github.com/alash3al/stash/internal/db"
	"github.com/alash3al/stash/internal/models"
	"github.com/alash3al/stash/internal/queries"
	"github.com/alash3al/stash/internal/reasoner"
	"github.com/pgvector/pgvector-go"
)

type fixedEmbedder struct{ vector []float32 }

func (e fixedEmbedder) Embed(context.Context, string) ([]float32, error) { return e.vector, nil }
func (e fixedEmbedder) Model() string                                    { return "test-embedding" }
func (e fixedEmbedder) Dims() int                                        { return len(e.vector) }

type noopReasoner struct{}

func (noopReasoner) ReasonStructured(context.Context, []string) (*reasoner.StructuredFact, error) {
	return nil, nil
}
func (noopReasoner) ReasonRelationships(context.Context, string) ([]*reasoner.StructuredRelationship, error) {
	return nil, nil
}
func (noopReasoner) ReasonPatterns(context.Context, []models.Fact, []models.Relationship) ([]*reasoner.StructuredPattern, error) {
	return nil, nil
}
func (noopReasoner) ReasonContradiction(context.Context, string, string, string, string) (*reasoner.ContradictionResult, error) {
	return nil, nil
}
func (noopReasoner) ReasonCausalLinks(context.Context, []models.Fact) ([]*reasoner.StructuredCausalLink, error) {
	return nil, nil
}
func (noopReasoner) ReasonGoalProgress(context.Context, []models.Goal, []models.Fact) ([]*reasoner.GoalProgressAssessment, error) {
	return nil, nil
}
func (noopReasoner) ReasonFailurePatterns(context.Context, []models.Failure, []string) ([]*reasoner.FailurePatternResult, error) {
	return nil, nil
}
func (noopReasoner) ReasonHypothesisEvidence(context.Context, []models.Hypothesis, []models.Fact) ([]*reasoner.HypothesisEvidenceResult, error) {
	return nil, nil
}

func TestRecallFeedbackLoopIntegration(t *testing.T) {
	dsn := os.Getenv("STASH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STASH_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, "test-embedding", 3)
	if err != nil {
		t.Fatal(err)
	}
	q, err := queries.New()
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	cfg := brain.DefaultConfig()
	cfg.RetrievalLearningEnabled = true
	cfg.RetrievalOverfetchFactor = 3
	cfg.RetrievalUtilityWeight = 0.08
	cfg.RetrievalMaxUtilityDelta = 0.10
	br, err := brain.New(pool, fixedEmbedder{vector: []float32{1, 0, 0}}, noopReasoner{}, q, cfg)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	defer br.Close()

	slug := fmt.Sprintf("/recall-loop-test-%d", time.Now().UnixNano())
	namespaceID, err := br.CreateNamespace(ctx, slug, "Recall loop test", "integration test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(ctx, "DELETE FROM namespaces WHERE id = $1", namespaceID) }()

	now := time.Now().UTC()
	insertFact := func(content string, vector []float32, confidence float32, validUntil *time.Time) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO facts
			 (namespace_id, content, embedding, embedding_model, confidence, valid_from, valid_until)
			 VALUES ($1, $2, $3, 'test-embedding', $4, $5, $6) RETURNING id`,
			namespaceID, content, pgvector.NewVector(vector), confidence, now, validUntil,
		).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	canonicalID := insertFact("canonical semantic leader", []float32{1, 0, 0}, 0.95, nil)
	helpfulID := insertFact("nearly equal but task helpful", []float32{0.9998, 0.02, 0}, 0.30, nil)
	expired := now.Add(-time.Hour)
	_ = insertFact("expired fact must never leak", []float32{1, 0, 0}, 1.0, &expired)

	query := "private integration query that must not be stored"
	first, err := br.RecallWithOptions(ctx, []string{slug}, query, 10, brain.RecallOptions{RecordOutcome: true, Caller: "integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("recall returned %d results, want 2 active facts: %+v", len(first), first)
	}
	if first[0].ID != canonicalID {
		t.Fatalf("initial semantic leader = %d, want %d", first[0].ID, canonicalID)
	}
	var helpful brain.RecallResult
	for _, result := range first {
		if result.ID == helpfulID {
			helpful = result
		}
		if result.Content == "expired fact must never leak" {
			t.Fatal("expired fact leaked into recall")
		}
	}
	if helpful.ImpressionID == 0 {
		t.Fatal("learning recall did not return an impression id")
	}

	feedback, err := br.RecordRecallFeedback(ctx, helpful.ImpressionID, "fact", helpful.ID, "helpful", "integration-idempotency-key", "helped finish the task")
	if err != nil {
		t.Fatal(err)
	}
	if !feedback.Recorded || feedback.HelpfulCount != 1 || feedback.UtilityScore <= 0 {
		t.Fatalf("unexpected first feedback result: %+v", feedback)
	}
	duplicate, err := br.RecordRecallFeedback(ctx, helpful.ImpressionID, "fact", helpful.ID, "helpful", "integration-idempotency-key", "retry")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Recorded || duplicate.HelpfulCount != 1 {
		t.Fatalf("duplicate feedback was not idempotent: %+v", duplicate)
	}
	if _, err := br.RecordRecallFeedback(ctx, helpful.ImpressionID, "fact", canonicalID, "helpful", "integration-idempotency-key", "wrong target"); !errors.Is(err, brain.ErrIdempotencyKeyReuse) {
		t.Fatalf("cross-target idempotency reuse error = %v, want ErrIdempotencyKeyReuse", err)
	}
	if _, err := br.RecordRecallFeedback(ctx, helpful.ImpressionID, "fact", helpful.ID, "helpful", "different-key-same-result", "duplicate vote"); err == nil {
		t.Fatal("a second signal for the same impression result was accepted")
	}

	second, err := br.RecallWithOptions(ctx, []string{slug}, query, 10, brain.RecallOptions{RecordOutcome: true, Caller: "integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].ID != helpfulID {
		t.Fatalf("helpful result did not receive bounded promotion: %+v", second)
	}
	if second[0].Score-second[0].SemanticScore > 0.100001 {
		t.Fatalf("utility promotion exceeded cap: %+v", second[0])
	}

	var confidence float32
	if err := pool.QueryRow(ctx, "SELECT confidence FROM facts WHERE id = $1", helpfulID).Scan(&confidence); err != nil {
		t.Fatal(err)
	}
	if confidence != 0.30 {
		t.Fatalf("feedback mutated canonical confidence: got %f", confidence)
	}

	var rawQueryColumnCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'recall_impressions' AND column_name IN ('query', 'query_text', 'raw_query')`,
	).Scan(&rawQueryColumnCount); err != nil {
		t.Fatal(err)
	}
	if rawQueryColumnCount != 0 {
		t.Fatal("recall impression schema contains a raw query column")
	}

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", namespaceID); err != nil {
		lockConn.Release()
		t.Fatal(err)
	}
	_, consolidateErr := br.ConsolidateByID(ctx, namespaceID)
	_, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", namespaceID)
	lockConn.Release()
	if !errors.Is(consolidateErr, brain.ErrConsolidationInProgress) {
		t.Fatalf("concurrent consolidation error = %v, want ErrConsolidationInProgress", consolidateErr)
	}
}
