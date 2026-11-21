package main

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/brain"
	"github.com/urfave/cli/v3"
)

func goalCreateCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("content argument is required")
	}
	content := args.First()
	namespace := cmd.String("namespace")
	priority := cmd.Int("priority")

	var parentID *int64
	if pid := cmd.Int("parent-id"); pid != 0 {
		pid64 := int64(pid)
		parentID = &pid64
	}

	bc := getBootstrap(cmd)
	nsIDs, err := bc.Brain.ResolveNamespaceIDs(ctx, []string{namespace})
	if err != nil {
		return err
	}

	g, err := bc.Brain.CreateGoal(ctx, nsIDs[0], content, parentID, priority)
	if err != nil {
		return err
	}
	return printJSON(g)
}

func goalListCmd(ctx context.Context, cmd *cli.Command) error {
	namespaces := cmd.StringSlice("namespaces")
	status := cmd.String("status")
	page := brain.Pagination{
		Offset: cmd.Int("offset"),
		Limit:  cmd.Int("limit"),
	}

	var parentID *int64
	if pid := cmd.Int("parent-id"); pid != 0 {
		pid64 := int64(pid)
		parentID = &pid64
	}

	bc := getBootstrap(cmd)
	goals, err := bc.Brain.ListGoals(ctx, namespaces, status, parentID, page)
	if err != nil {
		return err
	}
	return printJSON(goals)
}

func goalShowCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("goal ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid goal ID: %w", err)
	}

	bc := getBootstrap(cmd)
	g, err := bc.Brain.GetGoal(ctx, id)
	if err != nil {
		return err
	}

	total, completed, _ := bc.Brain.GetGoalProgress(ctx, id)

	return printJSON(map[string]any{
		"goal":      g,
		"sub_goals": map[string]int{"total": total, "completed": completed},
	})
}

func goalCompleteCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("goal ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid goal ID: %w", err)
	}

	notes := cmd.String("notes")

	bc := getBootstrap(cmd)
	g, err := bc.Brain.CompleteGoal(ctx, id, notes)
	if err != nil {
		return err
	}
	return printJSON(g)
}

func goalAbandonCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("goal ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid goal ID: %w", err)
	}

	notes := cmd.String("notes")

	bc := getBootstrap(cmd)
	g, err := bc.Brain.AbandonGoal(ctx, id, notes)
	if err != nil {
		return err
	}
	return printJSON(g)
}

func goalUpdateCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("goal ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid goal ID: %w", err)
	}

	content := cmd.String("content")
	priority := cmd.Int("priority")
	notes := cmd.String("notes")

	bc := getBootstrap(cmd)
	g, err := bc.Brain.UpdateGoal(ctx, id, content, priority, notes)
	if err != nil {
		return err
	}
	return printJSON(g)
}

func goalDeleteCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("goal ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid goal ID: %w", err)
	}

	bc := getBootstrap(cmd)
	if err := bc.Brain.DeleteGoal(ctx, id); err != nil {
		return err
	}
	return printJSON(map[string]string{"message": "Goal deleted successfully"})
}
