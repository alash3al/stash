package evals

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

/* ── Scoring arithmetic ──────────────────────────────────────────────────── */

func TestFirstRankIsOneBased(t *testing.T) {
	// MRR is defined on 1-based ranks. An off-by-one here silently inflates
	// every score in every report, so it is worth pinning explicitly.
	r := score([]int64{7}, []int64{7, 1, 2})
	if r.FirstRank != 1 {
		t.Fatalf("FirstRank = %d, want 1 for a top result", r.FirstRank)
	}
	if r.ReciprocalRank != 1.0 {
		t.Fatalf("ReciprocalRank = %v, want 1.0", r.ReciprocalRank)
	}
}

func TestReciprocalRankRewardsHigherPlacement(t *testing.T) {
	first := score([]int64{9}, []int64{9, 1, 2, 3})
	third := score([]int64{9}, []int64{1, 2, 9, 3})
	if first.ReciprocalRank <= third.ReciprocalRank {
		t.Fatal("a correct answer ranked 1st must score above the same answer ranked 3rd")
	}
	if math.Abs(third.ReciprocalRank-1.0/3.0) > 1e-9 {
		t.Fatalf("rank 3 reciprocal = %v, want 1/3", third.ReciprocalRank)
	}
}

func TestAnyRelevantCountsAsAHit(t *testing.T) {
	// The corpus is duplicate-heavy: one question is answered correctly by any
	// of several interchangeable facts. Requiring a specific id would measure
	// duplicate-lottery luck rather than retrieval quality.
	r := score([]int64{100, 101, 102}, []int64{5, 101, 7})
	if !r.Hit {
		t.Fatal("matching any member of the relevant set must count as a hit")
	}
	if r.FirstRank != 2 {
		t.Fatalf("FirstRank = %d, want 2", r.FirstRank)
	}
}

func TestFirstRankTracksTheEarliestRelevantNotTheLast(t *testing.T) {
	r := score([]int64{10, 20}, []int64{1, 10, 3, 20})
	if r.FirstRank != 2 {
		t.Fatalf("FirstRank = %d, want 2 — the earliest relevant result", r.FirstRank)
	}
	if r.RelevantFound != 2 {
		t.Fatalf("RelevantFound = %d, want 2", r.RelevantFound)
	}
	if math.Abs(r.ReciprocalRank-0.5) > 1e-9 {
		t.Fatalf("ReciprocalRank = %v, want 0.5 from the FIRST hit only", r.ReciprocalRank)
	}
}

func TestCompleteMissScoresZero(t *testing.T) {
	r := score([]int64{1, 2}, []int64{8, 9})
	if r.Hit || r.FirstRank != 0 || r.ReciprocalRank != 0 || r.RelevantFound != 0 {
		t.Fatalf("a complete miss must score zero across the board, got %+v", r)
	}
}

func TestEmptyResultsAreAMissNotACrash(t *testing.T) {
	r := score([]int64{1}, nil)
	if r.Hit || r.ReciprocalRank != 0 {
		t.Fatal("no results returned must score as a miss")
	}
}

/* ── Aggregation ─────────────────────────────────────────────────────────── */

func TestAggregateComputesRecallAndMRROverAllCases(t *testing.T) {
	results := []CaseResult{
		{CaseID: "a", Hit: true, FirstRank: 1, ReciprocalRank: 1.0},
		{CaseID: "b", Hit: true, FirstRank: 4, ReciprocalRank: 0.25},
		{CaseID: "c", Hit: false},
		{CaseID: "d", Hit: false},
	}
	rep := aggregate("embedding", "m", 10, results)
	if rep.Hits != 2 || rep.RecallAtK != 0.5 {
		t.Fatalf("recall = %v (hits %d), want 0.5 (2)", rep.RecallAtK, rep.Hits)
	}
	// MRR divides by ALL cases, not just the hits — misses must drag it down.
	if math.Abs(rep.MRR-0.3125) > 1e-9 {
		t.Fatalf("MRR = %v, want 0.3125 ((1.0+0.25)/4)", rep.MRR)
	}
	if len(rep.Misses) != 2 || rep.Misses[0] != "c" {
		t.Fatalf("misses = %v, want [c d]", rep.Misses)
	}
}

func TestAggregateOfNothingDoesNotDivideByZero(t *testing.T) {
	rep := aggregate("embedding", "m", 10, nil)
	if rep.RecallAtK != 0 || rep.MRR != 0 || rep.Cases != 0 {
		t.Fatalf("empty run must aggregate to zeros, got %+v", rep)
	}
}

/* ── Comparison ──────────────────────────────────────────────────────────── */

func TestCompareSurfacesWhatChangedNotJustTheMeans(t *testing.T) {
	baseline := aggregate("embedding", "bge", 10, []CaseResult{
		{CaseID: "fixed", Hit: false},
		{CaseID: "broken", Hit: true, FirstRank: 2, ReciprocalRank: 0.5},
		{CaseID: "better", Hit: true, FirstRank: 5, ReciprocalRank: 0.2},
		{CaseID: "worse", Hit: true, FirstRank: 1, ReciprocalRank: 1.0},
	})
	candidate := aggregate("embedding_shadow", "qwen", 10, []CaseResult{
		{CaseID: "fixed", Hit: true, FirstRank: 3, ReciprocalRank: 1.0 / 3.0},
		{CaseID: "broken", Hit: false},
		{CaseID: "better", Hit: true, FirstRank: 1, ReciprocalRank: 1.0},
		{CaseID: "worse", Hit: true, FirstRank: 6, ReciprocalRank: 1.0 / 6.0},
	})
	cmp := Compare(baseline, candidate)

	if len(cmp.FixedCases) != 1 || cmp.FixedCases[0] != "fixed" {
		t.Fatalf("FixedCases = %v, want [fixed]", cmp.FixedCases)
	}
	if len(cmp.BrokenCases) != 1 || cmp.BrokenCases[0] != "broken" {
		t.Fatalf("BrokenCases = %v, want [broken]", cmp.BrokenCases)
	}
	if len(cmp.RankImproved) != 1 || cmp.RankImproved[0] != "better" {
		t.Fatalf("RankImproved = %v, want [better]", cmp.RankImproved)
	}
	if len(cmp.RankWorsened) != 1 || cmp.RankWorsened[0] != "worse" {
		t.Fatalf("RankWorsened = %v, want [worse]", cmp.RankWorsened)
	}
	// Recall is identical here (3/4 both ways) — the point of the breakdown is
	// that an unchanged mean can still hide a case that regressed.
	if cmp.RecallDelta != 0 {
		t.Fatalf("RecallDelta = %v, want 0", cmp.RecallDelta)
	}
	if len(cmp.BrokenCases) == 0 {
		t.Fatal("an unchanged mean must still surface the broken case")
	}
}

/* ── Loading and validation ──────────────────────────────────────────────── */

func writeSet(t *testing.T, s Set) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "set.json")
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRejectsACaseWithNoCorrectAnswer(t *testing.T) {
	// A case with no relevant ids can never pass. Loading it would quietly
	// drag every score down and read as a model regression.
	path := writeSet(t, Set{Version: 1, Cases: []Case{
		{ID: "a", Query: "q", Namespace: "/x", RelevantFactIDs: []int64{1}},
		{ID: "b", Query: "q", Namespace: "/x"},
	}})
	if _, err := Load(path); err == nil {
		t.Fatal("a case with no relevant_fact_ids must be rejected at load")
	}
}

func TestLoadRejectsDuplicateCaseIDs(t *testing.T) {
	path := writeSet(t, Set{Version: 1, Cases: []Case{
		{ID: "dup", Query: "q", Namespace: "/x", RelevantFactIDs: []int64{1}},
		{ID: "dup", Query: "q2", Namespace: "/x", RelevantFactIDs: []int64{2}},
	}})
	if _, err := Load(path); err == nil {
		t.Fatal("duplicate case ids must be rejected — results would be ambiguous")
	}
}

func TestLoadRejectsAnEmptySet(t *testing.T) {
	path := writeSet(t, Set{Version: 1})
	if _, err := Load(path); err == nil {
		t.Fatal("an empty set must be rejected, not scored as a vacuous 100%")
	}
}

func TestLoadAcceptsAWellFormedSet(t *testing.T) {
	path := writeSet(t, Set{Version: 1, Name: "gold", Cases: []Case{
		{ID: "a", Query: "q", Namespace: "/x", RelevantFactIDs: []int64{1, 2}, Expect: "thing"},
	}})
	s, err := Load(path)
	if err != nil {
		t.Fatalf("well-formed set rejected: %v", err)
	}
	if len(s.Cases) != 1 || s.Cases[0].Expect != "thing" {
		t.Fatalf("round-trip lost data: %+v", s.Cases)
	}
}

func TestOnlyKnownVectorColumnsAreQueryable(t *testing.T) {
	// The column name is interpolated into SQL, so the allow-list is a
	// security boundary, not a convenience.
	for _, bad := range []string{"", "content", "embedding; DROP TABLE facts", "embedding_shadow2"} {
		if AllowedColumns[bad] {
			t.Fatalf("column %q must not be allowed", bad)
		}
	}
	for _, good := range []string{"embedding", "embedding_shadow"} {
		if !AllowedColumns[good] {
			t.Fatalf("column %q should be allowed", good)
		}
	}
}
