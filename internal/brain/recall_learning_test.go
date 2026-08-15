package brain

import (
	"math"
	"testing"
)

func TestRerankResultsBoundsAndTruthSeparation(t *testing.T) {
	results := []RecallResult{
		{ID: 1, Type: "fact", SemanticScore: 0.95, Score: 0.95, Confidence: 0.2},
		{ID: 2, Type: "fact", SemanticScore: 0.60, Score: 0.60, Confidence: 1.0},
	}
	utilities := map[memoryRef]float32{
		{Type: "fact", ID: 1}: -1,
		{Type: "fact", ID: 2}: 1,
	}

	rerankResults(results, utilities, 0.50, 0.10)

	for _, result := range results {
		delta := math.Abs(float64(result.Score - result.SemanticScore))
		if delta > 0.100001 {
			t.Fatalf("utility delta exceeded cap: got %.6f", delta)
		}
	}
	if results[0].Confidence != 0.2 || results[1].Confidence != 1.0 {
		t.Fatal("reranking mutated epistemic confidence")
	}
	if results[0].Score <= results[1].Score {
		t.Fatal("bounded utility displaced a substantially more relevant result")
	}
}

func TestRerankResultsZeroUtilityPreservesSemanticScores(t *testing.T) {
	results := []RecallResult{
		{ID: 1, Type: "episode", SemanticScore: 0.8, Score: 0.8},
		{ID: 2, Type: "fact", SemanticScore: 0.7, Score: 0.7},
	}
	rerankResults(results, nil, 0.08, 0.10)
	for _, result := range results {
		if result.Score != result.SemanticScore {
			t.Fatalf("zero utility changed semantic score: %+v", result)
		}
	}
}

func TestSortRecallResultsDeterministic(t *testing.T) {
	results := []RecallResult{
		{ID: 3, Type: "episode", Score: 0.8, SemanticScore: 0.8, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: 2, Type: "fact", Score: 0.8, SemanticScore: 0.8, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: 1, Type: "fact", Score: 0.9, SemanticScore: 0.9},
	}
	sortRecallResults(results)
	if results[0].ID != 1 || results[1].ID != 2 || results[2].ID != 3 {
		t.Fatalf("unexpected deterministic order: %+v", results)
	}
}
