package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alash3al/stash/internal/bootstrap"
	"github.com/alash3al/stash/internal/brain"
	"github.com/urfave/cli/v3"
)

func rememberCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("content argument is required")
	}
	content := args.First()
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("content cannot be empty")
	}

	namespace := cmd.String("namespace")
	var occurredAt *time.Time
	if oa := cmd.String("occurred-at"); oa != "" {
		t, err := time.Parse(time.RFC3339, oa)
		if err != nil {
			return fmt.Errorf("invalid occurred-at format: %w", err)
		}
		occurredAt = &t
	}

	bc := getBootstrap(cmd)
	id, err := bc.Brain.Remember(ctx, namespace, content, occurredAt)
	if err != nil {
		return err
	}

	output := map[string]any{"id": id, "message": "Memory remembered successfully"}
	return printJSON(output)
}

func recallCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("query argument is required")
	}
	query := args.First()
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("query cannot be empty")
	}

	namespaces := cmd.StringSlice("namespaces")
	limit := cmd.Int("limit")

	bc := getBootstrap(cmd)
	results, err := bc.Brain.RecallWithOptions(ctx, namespaces, query, limit, brain.RecallOptions{
		RecordOutcome: !cmd.Bool("no-learning"),
		Caller:        "cli",
	})
	if err != nil {
		return err
	}

	return printJSON(results)
}

func forgetCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("description argument is required")
	}
	query := args.First()
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("description cannot be empty")
	}

	namespaces := cmd.StringSlice("namespaces")
	bc := getBootstrap(cmd)
	if err := bc.Brain.ForgetEpisode(ctx, namespaces, query); err != nil {
		return err
	}

	return printJSON(map[string]string{"message": "Memory forgotten successfully"})
}

func purgeEpisodeCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("episode ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid episode ID: %w", err)
	}

	bc := getBootstrap(cmd)
	if err := bc.Brain.PurgeEpisode(ctx, id); err != nil {
		return err
	}
	return printJSON(map[string]string{"message": "Episode purged successfully"})
}

func purgeFactCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("fact ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid fact ID: %w", err)
	}

	bc := getBootstrap(cmd)
	if err := bc.Brain.PurgeFact(ctx, id); err != nil {
		return err
	}
	return printJSON(map[string]string{"message": "Fact purged successfully"})
}

func factsListCmd(ctx context.Context, cmd *cli.Command) error {
	namespaces := cmd.StringSlice("namespaces")
	var since, until *time.Time
	if s := cmd.String("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("invalid since format: %w", err)
		}
		since = &t
	}
	if u := cmd.String("until"); u != "" {
		t, err := time.Parse(time.RFC3339, u)
		if err != nil {
			return fmt.Errorf("invalid until format: %w", err)
		}
		until = &t
	}

	page := brain.Pagination{
		Offset: cmd.Int("offset"),
		Limit:  cmd.Int("limit"),
	}

	bc := getBootstrap(cmd)
	facts, err := bc.Brain.QueryFacts(ctx, namespaces, since, until, page)
	if err != nil {
		return err
	}
	return printJSON(facts)
}

func getBootstrap(cmd *cli.Command) *bootstrap.Context {
	bc, _ := cmd.Root().Metadata["bootstrapCtx"].(*bootstrap.Context)
	return bc
}

func printJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	fmt.Println(string(b))
	return nil
}
