package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/config"
	"github.com/roma2246/holdfast/internal/lint"
)

func (a *app) lintCmd() *cobra.Command {
	var (
		format    string
		strict    bool
		listRules bool
	)
	cmd := &cobra.Command{
		Use:   "lint [PATH]",
		Short: "Check agent instruction files (CLAUDE.md, AGENTS.md, .cursorrules)",
		Long: `Finds CLAUDE.md, AGENTS.md and .cursorrules in the root and in nested
directories and checks them with the rules HF001–HF009. All checks are local, no LLM calls.

Exit codes: 0 for no errors, 1 for errors (with --strict, warnings too), 2 for a failure.
To disable a rule: disable = ["HF007"] in holdfast.toml
or a <!-- holdfast-disable HF003 --> comment in the file.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if listRules {
				for _, r := range lint.Rules() {
					desc := ""
					if d, ok := r.(lint.Describer); ok { // проверка, реализует ли r интерфейс
						desc = d.Description()
					}
					fmt.Fprintf(a.out, "%s  %s\n", r.ID(), desc)
				}
				return nil
			}
			if format != "human" && format != "json" {
				return fmt.Errorf("unknown format %q (human or json)", format)
			}
			path := a.dir
			if len(args) == 1 {
				path = args[0]
			}
			return a.runLint(cmd.Context(), path, format, strict)
		},
	}
	cmd.Flags().StringVarP(&format, "format", "f", "human", "output format: human or json")
	cmd.Flags().BoolVar(&strict, "strict", false, "treat warnings as errors")
	cmd.Flags().BoolVar(&listRules, "list-rules", false, "show the list of rules")
	return cmd
}

func (a *app) runLint(ctx context.Context, path, format string, strict bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	root, single, err := lint.ResolveTarget(path)
	if err != nil {
		return err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	files := []string{single}
	if single == "" {
		if files, err = lint.Discover(root, cfg.Lint.Files); err != nil {
			return err
		}
	}

	findings, err := lint.Run(ctx, lint.NewRepo(root, cfg), files, lint.Rules())
	if err != nil {
		return err
	}
	if format == "json" {
		if err := lint.WriteJSON(a.out, len(files), findings); err != nil {
			return err
		}
	} else {
		if len(files) == 0 {
			fmt.Fprintf(a.out, "No instruction files (%v) found in %s\n", cfg.Lint.Files, root)
			return nil
		}
		lint.WriteHuman(a.out, len(files), findings)
	}

	s := lint.Summarize(len(files), findings)
	if s.Errors > 0 || (strict && s.Warnings > 0) {
		return exitError{ExitFindings}
	}
	return nil
}
