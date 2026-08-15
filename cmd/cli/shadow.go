package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

// Shadow embedding migration commands (TOL-297).
//
// These build and validate a SECOND embedding representation. None of them
// change what production recall reads — the read-path swap is a separate,
// deliberate act. `status` and `compare` are read-only; `run` writes only to
// embedding_shadow.

func shadowStatusCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	if bc.Shadow == nil {
		return fmt.Errorf("shadow embedding is not configured; set STASH_SHADOW_EMBEDDING_MODEL and STASH_DEEPINFRA_EMBEDDING_API_KEY")
	}
	progress, err := bc.Shadow.Progress(ctx, cmd.String("namespace"))
	if err != nil {
		return err
	}
	return printJSON(progress)
}

func shadowRunCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	if bc.Shadow == nil {
		return fmt.Errorf("shadow embedding is not configured; set STASH_SHADOW_EMBEDDING_MODEL and STASH_DEEPINFRA_EMBEDDING_API_KEY")
	}
	namespace := cmd.String("namespace")
	if namespace == "" {
		return fmt.Errorf("--namespace is required; migrate one namespace at a time so each wave stays reviewable")
	}

	if err := bc.Shadow.EnsureSchema(ctx); err != nil {
		return err
	}

	// A dry run answers "what would this cost" without spending anything.
	if cmd.Bool("dry-run") {
		progress, err := bc.Shadow.Progress(ctx, namespace)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"dry_run":   true,
			"namespace": namespace,
			"progress":  progress,
			"note":      "no embeddings were requested; re-run without --dry-run to migrate",
		})
	}

	// Bounded by --waves: each wave is at most one batch of provider calls, so
	// the worst case for a single command is waves × batch requests. Never let
	// this be unbounded — that is how a migration becomes an incident.
	waves := cmd.Int("waves")
	if waves <= 0 {
		waves = 1
	}

	var (
		totalEmbedded int
		totalFailed   int
		allErrors     []string
		remaining     int
		complete      bool
	)
	for i := 0; i < int(waves); i++ {
		if ctx.Err() != nil {
			break
		}
		res, err := bc.Shadow.MigrateWave(ctx, namespace)
		totalEmbedded += res.Embedded
		totalFailed += res.Failed
		allErrors = append(allErrors, res.Errors...)
		remaining = res.Remaining
		complete = res.Complete
		if err != nil {
			return fmt.Errorf("wave %d: %w (embedded %d before failing)", i+1, err, totalEmbedded)
		}
		if res.Complete || res.Embedded == 0 {
			break
		}
	}

	return printJSON(map[string]any{
		"namespace": namespace,
		"embedded":  totalEmbedded,
		"failed":    totalFailed,
		"errors":    allErrors,
		"remaining": remaining,
		"complete":  complete,
	})
}

func shadowCompareCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	if bc.Shadow == nil {
		return fmt.Errorf("shadow embedding is not configured; set STASH_SHADOW_EMBEDDING_MODEL and STASH_DEEPINFRA_EMBEDDING_API_KEY")
	}
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("query argument is required")
	}
	cmp, err := bc.Shadow.Compare(ctx, bc.Embedder, cmd.String("namespace"), args.First(), int(cmd.Int("limit")))
	if err != nil {
		return err
	}
	return printJSON(cmp)
}

func shadowCommand() *cli.Command {
	nsFlag := &cli.StringFlag{Name: "namespace", Aliases: []string{"n"}, Usage: "Namespace slug (omit or / for the whole corpus)"}
	return &cli.Command{
		Name:  "shadow",
		Usage: "Build and validate a second embedding representation without touching live recall",
		Commands: []*cli.Command{
			{
				Name:   "status",
				Usage:  "Report shadow-embedding coverage",
				Action: shadowStatusCmd,
				Flags:  []cli.Flag{nsFlag},
			},
			{
				Name:   "run",
				Usage:  "Populate embedding_shadow for one namespace, in bounded waves",
				Action: shadowRunCmd,
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "namespace", Aliases: []string{"n"}, Usage: "Namespace slug to migrate (required)"},
					&cli.IntFlag{Name: "waves", Aliases: []string{"w"}, Value: 1, Usage: "Maximum batches to run in this invocation"},
					&cli.BoolFlag{Name: "dry-run", Aliases: []string{"d"}, Usage: "Report what remains without embedding anything"},
				},
			},
			{
				Name:   "compare",
				Usage:  "Run one query against both representations and report the disagreement",
				Action: shadowCompareCmd,
				Flags: []cli.Flag{
					nsFlag,
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 10, Usage: "Results per representation"},
				},
			},
		},
	}
}
