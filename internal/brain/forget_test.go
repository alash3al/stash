package brain

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alash3al/stash/internal/db"
	"github.com/alash3al/stash/internal/embedder"
	"github.com/alash3al/stash/internal/models"
	"github.com/alash3al/stash/internal/queries"
	"github.com/alash3al/stash/internal/reasoner"
	"github.com/pgvector/pgvector-go"
)

func TestParseForgetIDs(t *testing.T) {
	ids, err := ParseForgetIDs("1, 2,3")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Fatalf("got %v", ids)
	}
	if _, err := ParseForgetIDs("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestForgetResultRowsAffected(t *testing.T) {
	r := ForgetResult{EpisodesAffected: 1, FactsAffected: 2}
	if r.RowsAffected != 0 {
		t.Fatalf("zero value RowsAffected should be 0 before commit path sets it")
	}
	r.RowsAffected = r.EpisodesAffected + r.FactsAffected
	if r.RowsAffected != 3 {
		t.Fatalf("want 3 rows, got %d", r.RowsAffected)
	}
}

// TestForgetEpisodeExpiresSingleSourceFact documents TS-816: forgetting an episode must
// soft-delete consolidated facts that only cited that episode.
func TestForgetEpisodeExpiresSingleSourceFact(t *testing.T) {
	dsn := os.Getenv("STASH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STASH_TEST_POSTGRES_DSN is not set")
	}

	ctx := context.Background()
	const vectorDim = 3 // must match recall_learning_integration_test (shared CI Postgres)
	pool, err := db.Open(ctx, dsn, "test-embedding", vectorDim)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer pool.Close()

	emb := &embedder.Fake{Dimensions: vectorDim}
	q, err := queries.New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(pool, emb, stubReasoner{}, q, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	nsSlug := fmt.Sprintf("/test/forget-%d", time.Now().UnixNano())
	var nsID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO namespaces (slug, name) VALUES ($1, $2) RETURNING id",
		nsSlug, "forget test",
	).Scan(&nsID); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	_, _ = pool.Exec(ctx, "INSERT INTO consolidation_progress (namespace_id) VALUES ($1) ON CONFLICT DO NOTHING", nsID)

	episodeText := "mc-canary-episode-unique-" + nsSlug
	factText := "mc-canary-fact-unique-" + nsSlug
	epVec, _ := emb.Embed(ctx, episodeText)
	factVec, _ := emb.Embed(ctx, factText)

	var episodeID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO episodes (namespace_id, content, embedding, embedding_model, occurred_at)
		 VALUES ($1, $2, $3, $4, now()) RETURNING id`,
		nsID, episodeText, pgvector.NewVector(epVec), emb.Model(),
	).Scan(&episodeID); err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	var factID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, valid_from)
		 VALUES ($1, $2, $3, $4, 0.9, now()) RETURNING id`,
		nsID, factText, pgvector.NewVector(factVec), emb.Model(),
	).Scan(&factID); err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO fact_sources (fact_id, episode_id) VALUES ($1, $2)",
		factID, episodeID,
	); err != nil {
		t.Fatalf("fact_sources: %v", err)
	}

	// Multi-source fact must survive when one episode is forgotten.
	episodeText2 := episodeText + "-alt"
	epVec2, _ := emb.Embed(ctx, episodeText2)
	var episodeID2 int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO episodes (namespace_id, content, embedding, embedding_model, occurred_at)
		 VALUES ($1, $2, $3, $4, now()) RETURNING id`,
		nsID, episodeText2, pgvector.NewVector(epVec2), emb.Model(),
	).Scan(&episodeID2); err != nil {
		t.Fatalf("insert episode2: %v", err)
	}
	sharedFactText := "mc-canary-shared-fact-" + nsSlug
	sharedVec, _ := emb.Embed(ctx, sharedFactText)
	var sharedFactID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, valid_from)
		 VALUES ($1, $2, $3, $4, 0.9, now()) RETURNING id`,
		nsID, sharedFactText, pgvector.NewVector(sharedVec), emb.Model(),
	).Scan(&sharedFactID); err != nil {
		t.Fatalf("insert shared fact: %v", err)
	}
	for _, eid := range []int64{episodeID, episodeID2} {
		if _, err := pool.Exec(ctx,
			"INSERT INTO fact_sources (fact_id, episode_id) VALUES ($1, $2)",
			sharedFactID, eid,
		); err != nil {
			t.Fatalf("shared fact_sources: %v", err)
		}
	}

	result, err := b.Forget(ctx, []string{nsSlug}, ForgetInput{
		TargetType: ForgetTargetEpisode,
		ID:         episodeID,
	})
	if err != nil {
		t.Fatalf("forget episode: %v", err)
	}
	if result.EpisodesAffected != 1 || result.FactsAffected != 1 {
		t.Fatalf("expected 1 episode and 1 orphan fact affected, got %+v", result)
	}

	var factDeleted bool
	if err := pool.QueryRow(ctx,
		"SELECT deleted_at IS NOT NULL FROM facts WHERE id = $1",
		factID,
	).Scan(&factDeleted); err != nil || !factDeleted {
		t.Fatalf("orphan fact should be soft-deleted, deleted=%v err=%v", factDeleted, err)
	}

	var sharedDeleted bool
	if err := pool.QueryRow(ctx,
		"SELECT deleted_at IS NOT NULL FROM facts WHERE id = $1",
		sharedFactID,
	).Scan(&sharedDeleted); err != nil || sharedDeleted {
		t.Fatalf("multi-source fact must remain live, deleted=%v err=%v", sharedDeleted, err)
	}
}

func TestForgetFactByID(t *testing.T) {
	dsn := os.Getenv("STASH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STASH_TEST_POSTGRES_DSN is not set")
	}

	ctx := context.Background()
	const vectorDim = 3 // must match recall_learning_integration_test (shared CI Postgres)
	pool, err := db.Open(ctx, dsn, "test-embedding", vectorDim)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer pool.Close()

	emb := &embedder.Fake{Dimensions: vectorDim}
	q, err := queries.New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(pool, emb, stubReasoner{}, q, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	nsSlug := fmt.Sprintf("/test/forget-fact-%d", time.Now().UnixNano())
	var nsID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO namespaces (slug, name) VALUES ($1, $2) RETURNING id", nsSlug, "t",
	).Scan(&nsID); err != nil {
		t.Fatal(err)
	}

	text := "standalone-fact-" + nsSlug
	vec, _ := emb.Embed(ctx, text)
	var factID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, valid_from)
		 VALUES ($1, $2, $3, $4, 0.5, now()) RETURNING id`,
		nsID, text, pgvector.NewVector(vec), emb.Model(),
	).Scan(&factID); err != nil {
		t.Fatal(err)
	}

	result, err := b.Forget(ctx, nil, ForgetInput{TargetType: ForgetTargetFact, ID: factID})
	if err != nil {
		t.Fatal(err)
	}
	if result.FactsAffected != 1 || result.RowsAffected != 1 {
		t.Fatalf("got %+v", result)
	}

	var embedding *pgvector.Vector
	if err := pool.QueryRow(ctx, "SELECT embedding FROM facts WHERE id = $1", factID).Scan(&embedding); err != nil {
		t.Fatal(err)
	}
	if embedding != nil {
		t.Fatalf("expected null embedding after forget, got %v", embedding)
	}
}

type stubReasoner struct{}

func (stubReasoner) ReasonStructured(context.Context, []string) (*reasoner.StructuredFact, error) {
	return nil, fmt.Errorf("not implemented")
}
func (stubReasoner) ReasonRelationships(context.Context, string) ([]*reasoner.StructuredRelationship, error) {
	return nil, fmt.Errorf("not implemented")
}
func (stubReasoner) ReasonPatterns(context.Context, []models.Fact, []models.Relationship) ([]*reasoner.StructuredPattern, error) {
	return nil, fmt.Errorf("not implemented")
}
func (stubReasoner) ReasonContradiction(context.Context, string, string, string, string) (*reasoner.ContradictionResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (stubReasoner) ReasonCausalLinks(context.Context, []models.Fact) ([]*reasoner.StructuredCausalLink, error) {
	return nil, fmt.Errorf("not implemented")
}
func (stubReasoner) ReasonGoalProgress(context.Context, []models.Goal, []models.Fact) ([]*reasoner.GoalProgressAssessment, error) {
	return nil, fmt.Errorf("not implemented")
}
func (stubReasoner) ReasonFailurePatterns(context.Context, []models.Failure, []string) ([]*reasoner.FailurePatternResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (stubReasoner) ReasonHypothesisEvidence(context.Context, []models.Hypothesis, []models.Fact) ([]*reasoner.HypothesisEvidenceResult, error) {
	return nil, fmt.Errorf("not implemented")
}
