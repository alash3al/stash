package main

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/brain"
	"github.com/urfave/cli/v3"
)

func namespaceCreateCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("namespace slug is required")
	}
	slug := args.First()
	name := cmd.String("name")
	if name == "" {
		name = slug
	}
	description := cmd.String("description")

	bc := getBootstrap(cmd)
	id, err := bc.Brain.CreateNamespace(ctx, slug, name, description)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"id": id, "slug": slug})
}

func namespaceListCmd(ctx context.Context, cmd *cli.Command) error {
	patterns := cmd.StringSlice("namespaces")
	page := brain.Pagination{
		Offset: cmd.Int("offset"),
		Limit:  cmd.Int("limit"),
	}
	bc := getBootstrap(cmd)
	namespaces, err := bc.Brain.ListNamespaces(ctx, patterns, page)
	if err != nil {
		return err
	}
	return printJSON(namespaces)
}

func namespacePersonaCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("namespace slug is required")
	}
	slug := args.First()
	persona := cmd.String("persona")

	bc := getBootstrap(cmd)
	if persona != "" {
		if err := bc.Brain.SetNamespacePersona(ctx, slug, persona); err != nil {
			return err
		}
		return printJSON(map[string]string{"message": "Persona updated successfully"})
	}

	ns, err := bc.Brain.GetNamespace(ctx, slug)
	if err != nil {
		return err
	}
	return printJSON(map[string]string{"persona": ns.Persona})
}
