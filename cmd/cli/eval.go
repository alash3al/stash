package main

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/evals"
	"github.com/urfave/cli/v3"
)

// Retrieval evaluation against a gold set (TOL-298).
//
// `shadow compare` reports how two models DISAGREE. That is not the same
// question as which one is RIGHT — two models can disagree completely and both
// be correct. These commands answer correctness instead, by scoring against
// queries whose right answers are known.
//
// All three commands are read-only.

func evalRunCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	set, err := evals.Load(cmd.String("set"))
	if err != nil {
		return err
	}
	runner, err := evals.NewRunner(bc.Pool)
	if err != nil {
		return err
	}

	column := cmd.String("column")
	emb := bc.Embedder
	if column == "embedding_shadow" {
		if bc.Shadow == nil {
			return fmt.Errorf("shadow embedding is not configured; cannot score the shadow column")
		}
		emb = bc.Shadow.Embedder()
	}

	// Refuse to score a set whose gold rows have drifted. A stale case scores
	// zero for reasons unrelated to the model and reads as a regression.
	if !cmd.Bool("skip-validate") {
		problems, err := runner.Validate(ctx, set, column)
		if err != nil {
			return err
		}
		if len(problems) > 0 {
			return fmt.Errorf("gold set is stale against column %s (%d problems); fix it or pass --skip-validate:\n  %v",
				column, len(problems), problems)
		}
	}

	rep, err := runner.Run(ctx, set, emb, column, int(cmd.Int("limit")))
	if err != nil {
		return err
	}
	if !cmd.Bool("verbose") {
		rep.Results = nil
	}
	return printJSON(rep)
}

func evalValidateCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	set, err := evals.Load(cmd.String("set"))
	if err != nil {
		return err
	}
	runner, err := evals.NewRunner(bc.Pool)
	if err != nil {
		return err
	}
	column := cmd.String("column")
	problems, err := runner.Validate(ctx, set, column)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"set":      set.Name,
		"cases":    len(set.Cases),
		"column":   column,
		"ok":       len(problems) == 0,
		"problems": problems,
	})
}

func evalCompareCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	if bc.Shadow == nil {
		return fmt.Errorf("shadow embedding is not configured; nothing to compare against")
	}
	set, err := evals.Load(cmd.String("set"))
	if err != nil {
		return err
	}
	runner, err := evals.NewRunner(bc.Pool)
	if err != nil {
		return err
	}

	if !cmd.Bool("skip-validate") {
		for _, col := range []string{"embedding", "embedding_shadow"} {
			problems, err := runner.Validate(ctx, set, col)
			if err != nil {
				return err
			}
			if len(problems) > 0 {
				return fmt.Errorf("gold set is stale against column %s (%d problems); fix it or pass --skip-validate:\n  %v",
					col, len(problems), problems)
			}
		}
	}

	k := int(cmd.Int("limit"))
	// Each column is scored with the embedder that built it.
	baseline, err := runner.Run(ctx, set, bc.Embedder, "embedding", k)
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	candidate, err := runner.Run(ctx, set, bc.Shadow.Embedder(), "embedding_shadow", k)
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}

	cmp := evals.Compare(baseline, candidate)
	if !cmd.Bool("verbose") {
		cmp.Baseline.Results = nil
		cmp.Candidate.Results = nil
	}
	return printJSON(cmp)
}

func evalCommand() *cli.Command {
	setFlag := &cli.StringFlag{
		Name: "set", Aliases: []string{"s"},
		Value: "evals/gold-retrieval-v1.json",
		Usage: "Path to the gold eval set",
	}
	limitFlag := &cli.IntFlag{Name: "limit", Aliases: []string{"k"}, Value: 10, Usage: "Results per query (the k in recall@k)"}
	colFlag := &cli.StringFlag{Name: "column", Aliases: []string{"c"}, Value: "embedding", Usage: "Vector column: embedding | embedding_shadow"}
	skipFlag := &cli.BoolFlag{Name: "skip-validate", Usage: "Score even if the gold set has drifted (results become unreliable)"}
	verboseFlag := &cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "Include per-case results"}

	return &cli.Command{
		Name:  "eval",
		Usage: "Score retrieval against a gold set of queries with known-correct answers",
		Commands: []*cli.Command{
			{
				Name:   "validate",
				Usage:  "Check the gold set still matches the corpus",
				Action: evalValidateCmd,
				Flags:  []cli.Flag{setFlag, colFlag},
			},
			{
				Name:   "run",
				Usage:  "Score one column: recall@k and MRR",
				Action: evalRunCmd,
				Flags:  []cli.Flag{setFlag, colFlag, limitFlag, skipFlag, verboseFlag},
			},
			{
				Name:   "compare",
				Usage:  "Score both columns and report which cases changed",
				Action: evalCompareCmd,
				Flags:  []cli.Flag{setFlag, limitFlag, skipFlag, verboseFlag},
			},
		},
	}
}
