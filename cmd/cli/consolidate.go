package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alash3al/stash/internal/brain"
	"github.com/urfave/cli/v3"
)

func consolidateRunCmd(ctx context.Context, cmd *cli.Command) error {
	namespaces := cmd.StringSlice("namespaces")
	dryRun := cmd.Bool("dry-run")

	bc := getBootstrap(cmd)

	if dryRun {
		return printJSON(map[string]any{
			"namespaces": namespaces,
			"status":     "dry-run requested",
		})
	}

	if len(namespaces) == 0 {
		namespaces = []string{"/"}
	}

	ids, err := bc.Brain.ResolveNamespaceIDs(ctx, namespaces)
	if err != nil {
		return fmt.Errorf("resolve namespaces: %w", err)
	}

	var results []brain.ConsolidationResult
	for _, id := range ids {
		result, err := bc.Brain.ConsolidateByID(ctx, id)
		if err != nil {
			log.Printf("Consolidation failed for namespace ID %d: %v", id, err)
			continue
		}
		results = append(results, result)
		bc.Logger.Info("consolidation completed", "result", result)
	}

	return printJSON(results)
}

func consolidateServeCmd(ctx context.Context, cmd *cli.Command) error {
	interval := cmd.Duration("interval")
	namespaces := cmd.StringSlice("namespaces")

	bc := getBootstrap(cmd)

	if len(namespaces) == 0 {
		namespaces = []string{"/"}
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Starting consolidation service with interval %s", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if updated, err := bc.Brain.BackfillMissingEmbeddings(ctx); err != nil {
				log.Printf("Embedding backfill deferred: %v", err)
			} else if updated > 0 {
				log.Printf("Embedding backfill updated %d memories", updated)
			}
			ids, err := bc.Brain.ResolveNamespaceIDs(ctx, namespaces)
			if err != nil {
				log.Printf("Failed to resolve namespaces: %v", err)
				continue
			}
			for _, id := range ids {
				result, err := bc.Brain.ConsolidateByID(ctx, id)
				if err != nil {
					log.Printf("Consolidation failed for namespace ID %d: %v", id, err)
					continue
				}
				b, _ := json.Marshal(result)
				log.Printf("Consolidation completed for %s: %s", result.Namespace, string(b))
			}
		case <-ctx.Done():
			log.Printf("Consolidation service shutting down")
			return nil
		}
	}
}
