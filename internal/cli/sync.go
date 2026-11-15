package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/bridge"
	"github.com/roma2246/holdfast/internal/config"
	"github.com/roma2246/holdfast/internal/lint"
)

func (a *app) syncCmd() *cobra.Command {
	var (
		yes          bool
		noHeuristics bool
		dryRun       bool
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Turn the prohibitions in CLAUDE.md into guard policy ([guard.synced])",
		Long: `Reads the instruction files and turns prohibitions into enforceable guard policies.

An explicit block is applied right away:
  <!-- holdfast
  deny-paths: migrations/**, .github/workflows/**
  ask-commands: docker compose down
  -->

Phrases like "never edit X", "don't touch X", "never run ` + "`X`" + `" are found
heuristically and get into the config only after you confirm them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSync(yes, noHeuristics, dryRun)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accept all heuristic rules that were found")
	cmd.Flags().BoolVar(&noHeuristics, "no-heuristics", false, "only explicit <!-- holdfast ... --> blocks")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the rules without changing holdfast.toml")
	return cmd
}

func (a *app) runSync(yes, noHeuristics, dryRun bool) error {
	root, err := a.repoRoot()
	if err != nil {
		return err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	files, err := lint.Discover(root, cfg.Lint.Files)
	if err != nil {
		return err
	}

	var found []bridge.Rule
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, f)
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		found = append(found, bridge.Extract(filepath.ToSlash(rel), lines)...)
	}
	found = bridge.Dedupe(found)

	// Ранее подтверждённые эвристические правила не спрашиваем повторно.
	confirmed := map[string]bool{}
	for _, r := range cfg.Guard.Synced.Rules {
		if r.Origin == bridge.OriginHeuristic {
			confirmed[r.Kind+"\x00"+r.Pattern] = true
		}
	}

	var accepted []config.SyncedRule
	var pending []bridge.Rule
	for _, r := range found {
		switch {
		case r.Origin == bridge.OriginExplicit:
			accepted = append(accepted, r.ToSynced())
		case noHeuristics:
		case confirmed[r.Kind+"\x00"+r.Pattern]:
			accepted = append(accepted, r.ToSynced())
		default:
			pending = append(pending, r)
		}
	}

	if len(accepted) > 0 {
		fmt.Fprintln(a.out, "Rules:")
		for _, r := range accepted {
			fmt.Fprintf(a.out, "  %-13s %-30s  %s (%s)\n", r.Kind, r.Pattern, r.Source, r.Origin)
		}
	}
	if len(pending) > 0 {
		fmt.Fprintln(a.out, "\nPossible prohibitions found in the text (heuristics):")
		for _, r := range pending {
			fmt.Fprintf(a.out, "  %s  \"%s\"\n    → %s %s\n", r.Source(), r.Text, r.Kind, r.Pattern)
			if dryRun {
				continue
			}
			if yes || a.confirm("    Add to the policy?") {
				accepted = append(accepted, r.ToSynced())
			}
		}
	}

	if dryRun {
		fmt.Fprintln(a.out, "\n--dry-run: holdfast.toml not changed")
		return nil
	}
	if len(accepted) == 0 && len(cfg.Guard.Synced.Rules) == 0 {
		fmt.Fprintln(a.out, "No prohibitions to sync were found")
		return nil
	}
	if err := config.WriteSynced(root, accepted); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "\nRules written: %d → %s [guard.synced]\n", len(accepted), config.FileName)
	return nil
}
