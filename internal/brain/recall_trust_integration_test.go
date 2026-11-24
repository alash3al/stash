package brain_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alash3al/stash/internal/brain"
	"github.com/alash3al/stash/internal/db"
	"github.com/alash3al/stash/internal/queries"
	"github.com/pgvector/pgvector-go"
)

func TestRecallTrustFloorAndDedupeIntegration(t *testing.T) {
	dsn := os.Getenv("STASH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STASH_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, "test-embedding", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q, err := queries.New()
	if err != nil {
		t.Fatal(err)
	}

	cfg := brain.DefaultConfig()
	cfg.RecallMinScore = brain.DefaultRecallMinScore
	br, err := brain.New(pool, fixedEmbedder{vector: []float32{1, 0, 0}}, noopReasoner{}, q, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()

	slug := fmt.Sprintf("/recall-trust-%d", time.Now().UnixNano())
	nsID, err := br.CreateNamespace(ctx, slug, "recall trust", "integration")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(ctx, "DELETE FROM namespaces WHERE id = $1", nsID) }()

	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	vecA := []float32{1, 0, 0}
	vecB := []float32{0.99, 0.05, 0}
	for _, row := range []struct {
		content string
		vec     []float32
		valid   time.Time
	}{
		{"paraphrase A", vecA, old},
		{"paraphrase B", vecB, now},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, valid_from)
			 VALUES ($1, $2, $3, 'test-embedding', 0.9, $4)`,
			nsID, row.content, pgvector.NewVector(row.vec), row.valid,
		); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := br.RecallWithOptions(ctx, []string{slug}, "trust integration query", 10, brain.RecallOptions{Caller: "integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.BelowThreshold {
		t.Fatal("expected surviving hits above the score floor")
	}
	if resp.DuplicatesCollapsed != 1 {
		t.Fatalf("expected one collapsed near-duplicate paraphrase, got %d", resp.DuplicatesCollapsed)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result after dedupe, got %d: %+v", len(resp.Results), resp.Results)
	}
	if resp.Results[0].ValidFrom != now.Format(time.RFC3339) {
		t.Fatalf("expected newest paraphrase to survive, got %+v", resp.Results[0])
	}

	cfgFloor := brain.DefaultConfig()
	cfgFloor.RecallMinScore = brain.DefaultRecallMinScore
	brFloor, err := brain.New(pool, fixedEmbedder{vector: []float32{0, 1, 0}}, noopReasoner{}, q, cfgFloor)
	if err != nil {
		t.Fatal(err)
	}
	defer brFloor.Close()
	slug2 := fmt.Sprintf("/recall-trust-floor-%d", time.Now().UnixNano())
	ns2, err := brFloor.CreateNamespace(ctx, slug2, "floor", "integration")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(ctx, "DELETE FROM namespaces WHERE id = $1", ns2) }()
	if _, err := pool.Exec(ctx,
		`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, valid_from)
		 VALUES ($1, 'orthogonal to query', $2, 'test-embedding', 0.5, $3)`,
		ns2, pgvector.NewVector([]float32{1, 0, 0}), now,
	); err != nil {
		t.Fatal(err)
	}
	noise, err := brFloor.RecallWithOptions(ctx, []string{slug2}, "mismatched query", 5, brain.RecallOptions{Caller: "integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	if !noise.BelowThreshold || len(noise.Results) != 0 {
		t.Fatalf("weak-only recall must return below_threshold with no hits, got %+v", noise)
	}
}

// TestRecallSupersessionDetectionIntegration exercises the real contradictions
// query and entity+property matching end to end: the older fact scores higher
// but the newer one must come first, with both dates exposed.
func TestRecallSupersessionDetectionIntegration(t *testing.T) {
	dsn := os.Getenv("STASH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STASH_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, "test-embedding", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q, err := queries.New()
	if err != nil {
		t.Fatal(err)
	}
	cfg := brain.DefaultConfig()
	cfg.RecallMinScore = brain.DefaultRecallMinScore
	br, err := brain.New(pool, fixedEmbedder{vector: []float32{1, 0, 0}}, noopReasoner{}, q, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()

	slug := fmt.Sprintf("/recall-supersede-%d", time.Now().UnixNano())
	nsID, err := br.CreateNamespace(ctx, slug, "supersession", "integration")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(ctx, "DELETE FROM namespaces WHERE id = $1", nsID) }()

	oldT := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newT := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	insert := func(content string, vec []float32, valid time.Time, entity, property string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, valid_from, entity, property)
			 VALUES ($1, $2, $3, 'test-embedding', 0.9, $4, $5, $6) RETURNING id`,
			nsID, content, pgvector.NewVector(vec), valid, entity, property,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Distinct embeddings (cosine < 0.92) so dedupe does not interfere. The
	// OLD fact is the closer match to the query.
	oldID := insert("sponsor tiers $500/$1000", []float32{1, 0, 0}, oldT, "spokanewire", "sponsor_tiers")
	newID := insert("plan v2 sponsor tiers", []float32{0.6, 0.8, 0}, newT, "spokanewire", "sponsor_tiers")

	// Case 1: entity+property match, with a contradiction row in INVERTED order.
	if _, err := pool.Exec(ctx,
		`INSERT INTO contradictions (namespace_id, old_fact_id, new_fact_id, entity, property, old_value, new_value)
		 VALUES ($1, $2, $3, 'spokanewire', 'sponsor_tiers', 'v2', 'v1')`,
		nsID, newID, oldID,
	); err != nil {
		t.Fatal(err)
	}

	resp, err := br.RecallWithOptions(ctx, []string{slug}, "sponsor pricing", 10, brain.RecallOptions{Caller: "integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected both facts, got %+v", resp.Results)
	}
	if resp.Results[0].ID != newID || resp.Results[1].ID != oldID {
		t.Fatalf("newer valid_from must rank first, got %+v", resp.Results)
	}
	for _, r := range resp.Results {
		if len(r.SupersessionDates) != 2 {
			t.Fatalf("both results must carry both dates, got %+v", r)
		}
	}
}
