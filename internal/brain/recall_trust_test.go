package brain

import (
	"testing"
	"time"
)

func TestApplyRecallMinScoreDropsNoiseAndSignalsEmpty(t *testing.T) {
	// Regression for TS-3963: without a floor, nonsense queries return 0.0002–0.014
	// cosine hits that agents treat as knowledge.
	in := []RecallResult{
		{ID: 1, Score: 0.012, Content: "noise"},
		{ID: 2, Score: 0.009, Content: "more noise"},
	}
	out, below := applyRecallMinScore(in, DefaultRecallMinScore)
	if !below || len(out) != 0 {
		t.Fatalf("expected below_threshold with empty results, got below=%v len=%d", below, len(out))
	}

	kept, below := applyRecallMinScore([]RecallResult{{ID: 3, Score: 0.9}}, DefaultRecallMinScore)
	if below || len(kept) != 1 {
		t.Fatalf("expected strong hit to survive floor, got below=%v %+v", below, kept)
	}

	passThrough, below := applyRecallMinScore(in, 0)
	if below || len(passThrough) != 2 {
		t.Fatalf("STASH_RECALL_MIN_SCORE=0 must disable the floor, got below=%v len=%d", below, len(passThrough))
	}
}

func TestDedupeRecallByEmbeddingKeepsNewestParaphrase(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	results := []RecallResult{
		{ID: 1, Type: "fact", Score: 0.99, ValidFrom: base.Format(time.RFC3339), Content: "old tiers"},
		{ID: 2, Type: "fact", Score: 0.98, ValidFrom: newer.Format(time.RFC3339), Content: "plan v2 tiers"},
	}
	embeddings := map[memoryRef][]float32{
		{Type: "fact", ID: 1}: {1, 0, 0},
		{Type: "fact", ID: 2}: {0.99, 0.01, 0},
	}
	out, collapsed := dedupeRecallByEmbedding(results, embeddings)
	if collapsed != 1 || len(out) != 1 {
		t.Fatalf("expected one collapsed duplicate, collapsed=%d len=%d", collapsed, len(out))
	}
	if out[0].ID != 2 {
		t.Fatalf("expected newer fact to survive dedupe, got %+v", out[0])
	}
}

func TestApplySupersessionRankingPromotesNewerFact(t *testing.T) {
	oldT := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newT := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	results := []RecallResult{
		{ID: 10, Type: "fact", ValidFrom: oldT.Format(time.RFC3339), Score: 0.99},
		{ID: 20, Type: "fact", ValidFrom: newT.Format(time.RFC3339), Score: 0.98},
	}
	meta := map[int64]factRecallMeta{
		10: {ValidFrom: &oldT},
		20: {ValidFrom: &newT},
	}
	applySupersessionRanking(results, []supersessionPair{{OlderID: 10, NewerID: 20}}, meta)
	if results[0].ID != 20 {
		t.Fatalf("newer superseding fact should rank first, order=%+v", results)
	}
	if len(results[0].SupersessionDates) != 2 || len(results[1].SupersessionDates) != 2 {
		t.Fatalf("both results must expose supersession_dates, got %+v", results)
	}
}

func TestBuildSupersessionPairsOrdersByTimeNotRowOrder(t *testing.T) {
	oldT := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newT := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	meta := map[int64]factRecallMeta{
		1: {ValidFrom: &oldT, Entity: "pricing", Property: "tiers"},
		2: {ValidFrom: &newT, Entity: "pricing", Property: "tiers"},
		3: {ValidFrom: &newT, Entity: "other", Property: "x"},
	}

	// Contradiction row recorded with inverted columns (old_fact_id is the
	// NEWER fact): effective time must still decide which side is older.
	pairs := buildSupersessionPairs([]int64{1, 3}, []contradictionRow{{OldID: 3, NewID: 1}}, meta)
	if len(pairs) != 1 || pairs[0].OlderID != 1 || pairs[0].NewerID != 3 {
		t.Fatalf("contradiction pair must order by time, got %+v", pairs)
	}

	// Same entity+property with no contradiction row; duplicate sources
	// (row + property match) must yield exactly one pair.
	pairs = buildSupersessionPairs([]int64{2, 1}, []contradictionRow{{OldID: 1, NewID: 2}}, meta)
	if len(pairs) != 1 || pairs[0].OlderID != 1 || pairs[0].NewerID != 2 {
		t.Fatalf("expected single deduped pair 1->2, got %+v", pairs)
	}
	pairs = buildSupersessionPairs([]int64{2, 1}, nil, meta)
	if len(pairs) != 1 || pairs[0].OlderID != 1 || pairs[0].NewerID != 2 {
		t.Fatalf("same entity+property must pair, got %+v", pairs)
	}

	// Unrelated facts (different entity/property, no row) produce no pair.
	if got := buildSupersessionPairs([]int64{2, 3}, nil, meta); len(got) != 0 {
		t.Fatalf("unrelated facts must not pair, got %+v", got)
	}
	// Empty entity/property is not a subject.
	blank := map[int64]factRecallMeta{5: {ValidFrom: &oldT}, 6: {ValidFrom: &newT}}
	if got := buildSupersessionPairs([]int64{5, 6}, nil, blank); len(got) != 0 {
		t.Fatalf("blank entity/property must not pair, got %+v", got)
	}
}
