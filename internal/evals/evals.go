// Package evals scores Stash retrieval against a gold set of queries whose
// correct answers are known.
//
// Why this exists: comparing two embedding models by result OVERLAP measures
// disagreement, not correctness. Two models can disagree completely and both be
// right, or agree completely and both be wrong. A migration decision needs
// ground truth, so this package supplies it — queries paired with the facts
// that genuinely answer them.
//
// The same precedent was set on Legal Advocate OS (2026-07-20), where
// readiness percentages were retired as unverified heuristics specifically
// pending a gold retrieval eval set.
package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/alash3al/stash/internal/embedder"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// Case is one query with known-correct answers.
//
// RelevantFactIDs is a SET, not a single id, because the corpus contains many
// near-duplicate consolidated facts — one question can be answered correctly by
// any of eight interchangeable rows. Scoring a single "right answer" against
// that corpus would measure duplicate-lottery luck rather than retrieval
// quality.
type Case struct {
	ID              string  `json:"id"`
	Query           string  `json:"query"`
	Namespace       string  `json:"namespace"`
	RelevantFactIDs []int64 `json:"relevant_fact_ids"`
	// Expect is a distinctive substring at least one relevant fact must
	// contain. Fact ids drift as consolidation rewrites rows; this makes a
	// stale case detectable instead of silently scoring zero forever.
	Expect string `json:"expect,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Set is a versioned collection of cases.
type Set struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Created string `json:"created"`
	Notes   string `json:"notes,omitempty"`
	Cases   []Case `json:"cases"`
}

// Load reads and validates a gold set. Validation is strict: a malformed case
// silently scoring zero would look like a model regression.
func Load(path string) (*Set, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("evals: read %s: %w", path, err)
	}
	var s Set
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("evals: parse %s: %w", path, err)
	}
	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("evals: %s contains no cases", path)
	}
	seen := map[string]bool{}
	for i, c := range s.Cases {
		switch {
		case c.ID == "":
			return nil, fmt.Errorf("evals: case %d has no id", i)
		case seen[c.ID]:
			return nil, fmt.Errorf("evals: duplicate case id %q", c.ID)
		case c.Query == "":
			return nil, fmt.Errorf("evals: case %q has no query", c.ID)
		case c.Namespace == "":
			return nil, fmt.Errorf("evals: case %q has no namespace", c.ID)
		case len(c.RelevantFactIDs) == 0:
			return nil, fmt.Errorf("evals: case %q has no relevant_fact_ids; a case with no correct answer can never pass", c.ID)
		}
		seen[c.ID] = true
	}
	return &s, nil
}

/* ── Scoring ─────────────────────────────────────────────────────────────── */

// CaseResult is one case's outcome against one retrieval configuration.
type CaseResult struct {
	CaseID string `json:"case_id"`
	Query  string `json:"query"`
	// Hit reports whether ANY relevant fact appeared in the returned results.
	Hit bool `json:"hit"`
	// FirstRank is the 1-based position of the first relevant fact, 0 if none.
	FirstRank      int     `json:"first_rank"`
	ReciprocalRank float64 `json:"reciprocal_rank"`
	// RelevantFound counts how many of the relevant set were returned. Useful
	// context, but NOT the headline metric: with duplicate-heavy content,
	// returning eight copies of the right answer is not eight times better.
	RelevantFound int     `json:"relevant_found"`
	Returned      []int64 `json:"returned"`
}

// score evaluates one ranked result list against a relevant set.
//
// Kept pure and free of SQL so the metric arithmetic is exactly testable. Rank
// is 1-based because MRR is defined that way; an off-by-one here would quietly
// inflate every score.
func score(relevant, returned []int64) CaseResult {
	rel := make(map[int64]bool, len(relevant))
	for _, id := range relevant {
		rel[id] = true
	}
	res := CaseResult{Returned: returned}
	for i, id := range returned {
		if !rel[id] {
			continue
		}
		res.RelevantFound++
		if res.FirstRank == 0 {
			res.FirstRank = i + 1
			res.Hit = true
			res.ReciprocalRank = 1.0 / float64(res.FirstRank)
		}
	}
	return res
}

// Report aggregates a whole run.
type Report struct {
	Column string `json:"column"`
	Model  string `json:"model"`
	K      int    `json:"k"`
	Cases  int    `json:"cases"`
	Hits   int    `json:"hits"`
	// RecallAtK is the fraction of cases where a correct answer appeared at
	// all. This is the headline number for a known-item retrieval task.
	RecallAtK float64 `json:"recall_at_k"`
	// MRR rewards ranking the correct answer higher, not merely including it.
	MRR     float64      `json:"mrr"`
	Misses  []string     `json:"misses,omitempty"`
	Results []CaseResult `json:"results,omitempty"`
}

func aggregate(column, model string, k int, results []CaseResult) Report {
	rep := Report{Column: column, Model: model, K: k, Cases: len(results), Results: results}
	var rrSum float64
	for _, r := range results {
		if r.Hit {
			rep.Hits++
			rrSum += r.ReciprocalRank
		} else {
			rep.Misses = append(rep.Misses, r.CaseID)
		}
	}
	if len(results) > 0 {
		rep.RecallAtK = float64(rep.Hits) / float64(len(results))
		rep.MRR = rrSum / float64(len(results))
	}
	sort.Strings(rep.Misses)
	return rep
}

/* ── Runner ──────────────────────────────────────────────────────────────── */

// Runner executes a gold set against one embedding column.
type Runner struct {
	pool *pgxpool.Pool
}

func NewRunner(pool *pgxpool.Pool) (*Runner, error) {
	if pool == nil {
		return nil, fmt.Errorf("evals: pool is required")
	}
	return &Runner{pool: pool}, nil
}

// AllowedColumns are the only vector columns a run may target. An allow-list
// rather than free text because the column name is interpolated into SQL.
var AllowedColumns = map[string]bool{
	"embedding":        true,
	"embedding_shadow": true,
}

// Run scores every case in the set against one column, using emb to embed the
// queries. The embedder and the column must correspond: embedding a query with
// Qwen and searching the bge-m3 column compares incomparable vectors and
// produces meaningless numbers, so callers must pair them deliberately.
func (r *Runner) Run(ctx context.Context, set *Set, emb embedder.Embedder, column string, k int) (Report, error) {
	if !AllowedColumns[column] {
		return Report{}, fmt.Errorf("evals: column %q is not allowed", column)
	}
	if k <= 0 {
		k = 10
	}

	results := make([]CaseResult, 0, len(set.Cases))
	for _, c := range set.Cases {
		vec, err := embedder.EmbedQuery(ctx, emb, c.Query)
		if err != nil {
			return Report{}, fmt.Errorf("evals: embed case %q: %w", c.ID, err)
		}
		q := fmt.Sprintf(`
			SELECT id FROM facts
			 WHERE %s IS NOT NULL AND deleted_at IS NULL
			   AND namespace_id = (SELECT id FROM namespaces WHERE slug = $3)
			 ORDER BY %s <=> $1
			 LIMIT $2`, column, column)
		rows, err := r.pool.Query(ctx, q, pgvector.NewVector(vec), k, c.Namespace)
		if err != nil {
			return Report{}, fmt.Errorf("evals: query case %q: %w", c.ID, err)
		}
		var returned []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return Report{}, fmt.Errorf("evals: scan case %q: %w", c.ID, err)
			}
			returned = append(returned, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return Report{}, fmt.Errorf("evals: rows case %q: %w", c.ID, err)
		}
		rows.Close()

		cr := score(c.RelevantFactIDs, returned)
		cr.CaseID, cr.Query = c.ID, c.Query
		results = append(results, cr)
	}
	return aggregate(column, emb.Model(), k, results), nil
}

// Validate checks that every case's relevant facts still exist, are not
// deleted, live in the stated namespace, and have a vector in the target
// column — and that Expect, where given, still matches.
//
// Run this BEFORE trusting a score. A case whose gold rows were consolidated
// away scores zero for a reason that has nothing to do with the model, and
// would read as a regression.
func (r *Runner) Validate(ctx context.Context, set *Set, column string) ([]string, error) {
	if !AllowedColumns[column] {
		return nil, fmt.Errorf("evals: column %q is not allowed", column)
	}
	var problems []string
	for _, c := range set.Cases {
		q := fmt.Sprintf(`
			SELECT count(*) FILTER (WHERE %s IS NOT NULL),
			       count(*),
			       count(*) FILTER (WHERE content ILIKE '%%' || $3 || '%%')
			  FROM facts
			 WHERE id = ANY($1) AND deleted_at IS NULL
			   AND namespace_id = (SELECT id FROM namespaces WHERE slug = $2)`, column)
		var withVec, alive, matching int
		if err := r.pool.QueryRow(ctx, q, c.RelevantFactIDs, c.Namespace, c.Expect).Scan(&withVec, &alive, &matching); err != nil {
			return nil, fmt.Errorf("evals: validate case %q: %w", c.ID, err)
		}
		if alive == 0 {
			problems = append(problems, fmt.Sprintf("%s: none of its %d relevant facts still exist in %s", c.ID, len(c.RelevantFactIDs), c.Namespace))
			continue
		}
		if withVec == 0 {
			problems = append(problems, fmt.Sprintf("%s: no relevant fact has a %s vector — namespace not migrated?", c.ID, column))
		}
		if alive < len(c.RelevantFactIDs) {
			problems = append(problems, fmt.Sprintf("%s: %d of %d relevant facts are gone (consolidated or deleted)", c.ID, len(c.RelevantFactIDs)-alive, len(c.RelevantFactIDs)))
		}
		if c.Expect != "" && matching == 0 {
			problems = append(problems, fmt.Sprintf("%s: expect %q matches none of its relevant facts — content drifted", c.ID, c.Expect))
		}
	}
	return problems, nil
}

// Compare puts two reports side by side. Deliberately returns deltas and the
// cases that changed rather than a winner: which model is better is a judgement
// about the misses, not a threshold on a mean.
type Comparison struct {
	Baseline     Report   `json:"baseline"`
	Candidate    Report   `json:"candidate"`
	RecallDelta  float64  `json:"recall_delta"`
	MRRDelta     float64  `json:"mrr_delta"`
	FixedCases   []string `json:"fixed_cases"`   // candidate found, baseline missed
	BrokenCases  []string `json:"broken_cases"`  // baseline found, candidate missed
	RankImproved []string `json:"rank_improved"` // both found, candidate ranked higher
	RankWorsened []string `json:"rank_worsened"`
}

func Compare(baseline, candidate Report) Comparison {
	cmp := Comparison{
		Baseline:    baseline,
		Candidate:   candidate,
		RecallDelta: candidate.RecallAtK - baseline.RecallAtK,
		MRRDelta:    candidate.MRR - baseline.MRR,
	}
	base := map[string]CaseResult{}
	for _, r := range baseline.Results {
		base[r.CaseID] = r
	}
	for _, cand := range candidate.Results {
		b, ok := base[cand.CaseID]
		if !ok {
			continue
		}
		switch {
		case cand.Hit && !b.Hit:
			cmp.FixedCases = append(cmp.FixedCases, cand.CaseID)
		case !cand.Hit && b.Hit:
			cmp.BrokenCases = append(cmp.BrokenCases, cand.CaseID)
		case cand.Hit && b.Hit && cand.FirstRank < b.FirstRank:
			cmp.RankImproved = append(cmp.RankImproved, cand.CaseID)
		case cand.Hit && b.Hit && cand.FirstRank > b.FirstRank:
			cmp.RankWorsened = append(cmp.RankWorsened, cand.CaseID)
		}
	}
	for _, s := range [][]string{cmp.FixedCases, cmp.BrokenCases, cmp.RankImproved, cmp.RankWorsened} {
		sort.Strings(s)
	}
	return cmp
}
