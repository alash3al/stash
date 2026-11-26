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

func TestStatsIntegration(t *testing.T) {
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
	cfg.EmbedderInfo = brain.NewProviderInfo("https://api.deepinfra.com/v1/openai", "embed-model")
	cfg.ReasonerInfo = brain.NewProviderInfo("https://gateway.ai.cloudflare.com/v1/x", "reason-model")
	br, err := brain.New(pool, fixedEmbedder{vector: []float32{1, 0, 0}}, noopReasoner{}, q, cfg)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	defer br.Close()

	before, err := br.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	slug := fmt.Sprintf("/stats-test-%d", time.Now().UnixNano())
	nsID, err := br.CreateNamespace(ctx, slug, "Stats test", "integration test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(ctx, "DELETE FROM namespaces WHERE id = $1", nsID) }()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	vec := pgvector.NewVector([]float32{1, 0, 0})
	past := time.Now().Add(-time.Hour)

	// facts: 2 live, 1 expired, 1 soft-deleted. 100 more live facts would make
	// this namespace outrank anything else in a shared test database.
	for i := 0; i < 2; i++ {
		exec(`INSERT INTO facts (namespace_id, content, embedding, embedding_model) VALUES ($1, $2, $3, 'test-embedding')`, nsID, fmt.Sprintf("live %d", i), vec)
	}
	exec(`INSERT INTO facts (namespace_id, content, valid_until) VALUES ($1, 'expired', $2)`, nsID, past)
	exec(`INSERT INTO facts (namespace_id, content, deleted_at) VALUES ($1, 'deleted', now())`, nsID)
	exec(`INSERT INTO facts (namespace_id, content) SELECT $1, 'bulk ' || g FROM generate_series(1, 100) g`, nsID)

	// episodes: consolidated one, then an old embedded one and a fresh unembedded one behind the watermark.
	var consolidatedID int64
	if err := pool.QueryRow(ctx, `INSERT INTO episodes (namespace_id, content, embedding, embedding_model) VALUES ($1, 'done', $2, 'test-embedding') RETURNING id`, nsID, vec).Scan(&consolidatedID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO episodes (namespace_id, content, embedding, embedding_model, created_at) VALUES ($1, 'old', $2, 'test-embedding', now() - interval '48 hours')`, nsID, vec)
	exec(`INSERT INTO episodes (namespace_id, content) VALUES ($1, 'unembedded')`, nsID)
	exec(`INSERT INTO consolidation_progress (namespace_id, last_episode_id) VALUES ($1, $2)
	      ON CONFLICT (namespace_id) DO UPDATE SET last_episode_id = EXCLUDED.last_episode_id`, nsID, consolidatedID)

	exec(`INSERT INTO contexts (namespace_id, focus, expires_at) VALUES ($1, 'x', $2)`, nsID, past)
	exec(`INSERT INTO relationships (namespace_id, from_entity, relation_type, to_entity) VALUES ($1, 'a', 'r', 'b')`, nsID)

	after, err := br.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	delta := func(name string, got, want int64) {
		t.Helper()
		if got != want {
			t.Errorf("%s delta = %d, want %d", name, got, want)
		}
	}
	delta("namespaces", after.Counts.Namespaces-before.Counts.Namespaces, 1)
	delta("episodes", after.Counts.Episodes-before.Counts.Episodes, 3)
	delta("facts.live", after.Counts.Facts.Live-before.Counts.Facts.Live, 102)
	delta("facts.expired", after.Counts.Facts.Expired-before.Counts.Facts.Expired, 1)
	delta("facts.soft_deleted", after.Counts.Facts.Deleted-before.Counts.Facts.Deleted, 1)
	delta("relationships", after.Counts.Relationships-before.Counts.Relationships, 1)
	delta("contexts.expired", after.Counts.Contexts.Expired-before.Counts.Contexts.Expired, 1)
	delta("contexts.live", after.Counts.Contexts.Live-before.Counts.Contexts.Live, 0)
	delta("backlog", after.Consolidation.UnconsolidatedEpisodes-before.Consolidation.UnconsolidatedEpisodes, 2)
	delta("unembedded", after.Consolidation.UnembeddedEpisodes-before.Consolidation.UnembeddedEpisodes, 1)
	delta("never consolidated", after.Consolidation.NamespacesNeverConsolidated-before.Consolidation.NamespacesNeverConsolidated, 1)

	if after.Consolidation.OldestEpisodeAgeSecs == nil || *after.Consolidation.OldestEpisodeAgeSecs < int64((47*time.Hour).Seconds()) {
		t.Errorf("oldest unconsolidated age = %v, want >= 47h", after.Consolidation.OldestEpisodeAgeSecs)
	}

	if len(after.TopNamespaces) == 0 || len(after.TopNamespaces) > brain.StatsTopNamespaces {
		t.Fatalf("top namespaces len = %d, want 1..%d", len(after.TopNamespaces), brain.StatsTopNamespaces)
	}
	for i := 1; i < len(after.TopNamespaces); i++ {
		if after.TopNamespaces[i].Facts > after.TopNamespaces[i-1].Facts {
			t.Errorf("top namespaces not sorted by facts desc at %d", i)
		}
	}
	for _, ns := range after.TopNamespaces {
		if ns.Namespace == slug {
			if ns.Facts != 103 || ns.LiveFacts != 102 || ns.UnconsolidatedEpisodes != 2 {
				t.Errorf("namespace row = %+v, want facts=103 live=102 unconsolidated=2", ns)
			}
		}
	}

	if after.Embedder.Provider != "deepinfra" || after.Embedder.CloudflareHost {
		t.Errorf("embedder info = %+v", after.Embedder)
	}
	if after.Reasoner.BaseURLHost != "gateway.ai.cloudflare.com" || !after.Reasoner.CloudflareHost {
		t.Errorf("reasoner info = %+v", after.Reasoner)
	}
}
