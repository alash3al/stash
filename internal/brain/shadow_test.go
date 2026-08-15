package brain

import (
	"context"
	"errors"
	"testing"

	"github.com/alash3al/stash/internal/embedder"
)

// stubEmbedder is a minimal Embedder for construction-time tests.
type stubEmbedder struct{ dims int }

func (s *stubEmbedder) Embed(context.Context, string) ([]float32, error) {
	return make([]float32, s.dims), nil
}
func (s *stubEmbedder) Dims() int     { return s.dims }
func (s *stubEmbedder) Model() string { return "stub/model" }

var _ embedder.Embedder = (*stubEmbedder)(nil)

/* ── Construction guards ─────────────────────────────────────────────────── */

func TestShadowMigratorRefusesDimensionsPgvectorCannotIndex(t *testing.T) {
	// The whole point of requesting 1024 from an MRL model is staying under
	// pgvector's 2000-dim HNSW ceiling for the `vector` type. Catching this at
	// construction is the difference between an error and discovering it after
	// embedding 15k rows that then cannot be indexed.
	_, err := NewShadowMigrator(nil, &stubEmbedder{dims: 4096}, "Qwen/Qwen3-Embedding-8B", 4096, 100)
	if err == nil {
		t.Fatal("4096 dims must be refused: pgvector cannot build an HNSW index on vector(4096)")
	}
}

func TestShadowMigratorUnconfiguredIsADistinctError(t *testing.T) {
	// Callers must be able to tell "not set up" from "broken", because the
	// first is a normal production state and the second is an incident.
	_, err := NewShadowMigrator(nil, nil, "", 1024, 100)
	if !errors.Is(err, ErrShadowNotConfigured) {
		t.Fatalf("got %v, want ErrShadowNotConfigured", err)
	}

	_, err = NewShadowMigrator(nil, &stubEmbedder{dims: 1024}, "", 1024, 100)
	if !errors.Is(err, ErrShadowNotConfigured) {
		t.Fatalf("a model-less migrator: got %v, want ErrShadowNotConfigured", err)
	}
}

func TestShadowMigratorRejectsNonPositiveDimensions(t *testing.T) {
	if _, err := NewShadowMigrator(nil, &stubEmbedder{dims: 1024}, "m", 0, 100); err == nil {
		t.Fatal("zero dimensions must be rejected")
	}
	if _, err := NewShadowMigrator(nil, &stubEmbedder{dims: 1024}, "m", -1, 100); err == nil {
		t.Fatal("negative dimensions must be rejected")
	}
}

/* ── Comparison arithmetic — the migration go/no-go evidence ─────────────── */

func TestIdenticalResultsAreFullOverlap(t *testing.T) {
	ids := []int64{1, 2, 3}
	overlap, liveOnly, shadowOnly, ratio := compareIDSets(ids, ids)
	if overlap != 3 || ratio != 1.0 {
		t.Fatalf("overlap=%d ratio=%v, want 3 and 1.0", overlap, ratio)
	}
	if len(liveOnly) != 0 || len(shadowOnly) != 0 {
		t.Fatalf("identical sets must have no exclusives, got live=%v shadow=%v", liveOnly, shadowOnly)
	}
}

func TestOverlapIgnoresRankOrder(t *testing.T) {
	// Two models ranking the same documents differently still agree on WHAT is
	// relevant. Reporting that as disagreement would block good migrations.
	overlap, liveOnly, shadowOnly, ratio := compareIDSets([]int64{1, 2, 3}, []int64{3, 1, 2})
	if overlap != 3 || ratio != 1.0 {
		t.Fatalf("reordered identical sets: overlap=%d ratio=%v, want 3 and 1.0", overlap, ratio)
	}
	if len(liveOnly) != 0 || len(shadowOnly) != 0 {
		t.Fatal("reordering is not disagreement")
	}
}

func TestDisjointResultsSurfaceBothSides(t *testing.T) {
	overlap, liveOnly, shadowOnly, ratio := compareIDSets([]int64{1, 2}, []int64{8, 9})
	if overlap != 0 || ratio != 0 {
		t.Fatalf("overlap=%d ratio=%v, want 0 and 0", overlap, ratio)
	}
	if len(liveOnly) != 2 || len(shadowOnly) != 2 {
		t.Fatalf("both exclusives must be reported for a human to judge, got live=%v shadow=%v", liveOnly, shadowOnly)
	}
}

func TestRatioIsMeasuredAgainstLiveNotTheUnion(t *testing.T) {
	// The question is "how much of today's production behaviour survives",
	// so the denominator is the live set.
	overlap, _, shadowOnly, ratio := compareIDSets([]int64{1, 2, 3, 4}, []int64{1, 2, 99})
	if overlap != 2 {
		t.Fatalf("overlap = %d, want 2", overlap)
	}
	if ratio != 0.5 {
		t.Fatalf("ratio = %v, want 0.5 (2 of 4 live results retained)", ratio)
	}
	if len(shadowOnly) != 1 || shadowOnly[0] != 99 {
		t.Fatalf("shadow-only = %v, want [99]", shadowOnly)
	}
}

func TestEmptyLiveSetDoesNotDivideByZero(t *testing.T) {
	overlap, liveOnly, shadowOnly, ratio := compareIDSets(nil, []int64{1})
	if overlap != 0 || ratio != 0 {
		t.Fatalf("overlap=%d ratio=%v, want zeros", overlap, ratio)
	}
	if len(liveOnly) != 0 {
		t.Fatal("no live results means no live-only results")
	}
	if len(shadowOnly) != 1 {
		t.Fatalf("shadow-only = %v, want [1]", shadowOnly)
	}
}

func TestBothEmptyIsNotAnError(t *testing.T) {
	overlap, liveOnly, shadowOnly, ratio := compareIDSets(nil, nil)
	if overlap != 0 || ratio != 0 || liveOnly != nil || shadowOnly != nil {
		t.Fatal("two empty result sets must compare cleanly to zero")
	}
}
