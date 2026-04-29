package main

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/brain"
	"github.com/urfave/cli/v3"
)

func failureCreateCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("content argument is required")
	}
	content := args.First()
	reason := cmd.String("reason")
	lesson := cmd.String("lesson")
	namespace := cmd.String("namespace")

	if reason == "" {
		return fmt.Errorf("--reason is required")
	}
	if lesson == "" {
		return fmt.Errorf("--lesson is required")
	}

	var goalID *int64
	if gid := cmd.Int("goal-id"); gid != 0 {
		gid64 := int64(gid)
		goalID = &gid64
	}

	bc := getBootstrap(cmd)
	nsIDs, err := bc.Brain.ResolveNamespaceIDs(ctx, []string{namespace})
	if err != nil {
		return err
	}

	f, err := bc.Brain.CreateFailure(ctx, nsIDs[0], content, reason, lesson, goalID)
	if err != nil {
		return err
	}
	return printJSON(f)
}

func failureListCmd(ctx context.Context, cmd *cli.Command) error {
	namespaces := cmd.StringSlice("namespaces")
	page := brain.Pagination{
		Offset: cmd.Int("offset"),
		Limit:  cmd.Int("limit"),
	}

	var goalID *int64
	if gid := cmd.Int("goal-id"); gid != 0 {
		gid64 := int64(gid)
		goalID = &gid64
	}

	bc := getBootstrap(cmd)
	failures, err := bc.Brain.ListFailures(ctx, namespaces, goalID, page)
	if err != nil {
		return err
	}
	return printJSON(failures)
}

func failureShowCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("failure ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid failure ID: %w", err)
	}

	bc := getBootstrap(cmd)
	f, err := bc.Brain.GetFailure(ctx, id)
	if err != nil {
		return err
	}
	return printJSON(f)
}

func failureDeleteCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("failure ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid failure ID: %w", err)
	}

	bc := getBootstrap(cmd)
	if err := bc.Brain.DeleteFailure(ctx, id); err != nil {
		return err
	}
	return printJSON(map[string]string{"message": "Failure deleted successfully"})
}
func failureTriageCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("failure ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid failure ID: %w", err)
	}

	bc := getBootstrap(cmd)
	if err := bc.Brain.TriageFailure(ctx, id); err != nil {
		return err
	}
	return printJSON(map[string]string{"message": "Failure triaged successfully"})
}
