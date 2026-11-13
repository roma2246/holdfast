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
		Short: "Перенести запреты из CLAUDE.md в политику guard ([guard.synced])",
		Long: `Читает файлы инструкций и превращает запреты в проверяемые политики guard.

Явный блок применяется сразу:
  <!-- holdfast
  deny-paths: migrations/**, .github/workflows/**
  ask-commands: docker compose down
  -->

Фразы вида «never edit X», «не трогай X», «never run ` + "`X`" + `» находятся
эвристически и попадают в конфиг только после вашего подтверждения.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSync(yes, noHeuristics, dryRun)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "принять все найденные эвристические правила")
	cmd.Flags().BoolVar(&noHeuristics, "no-heuristics", false, "только явные блоки <!-- holdfast ... -->")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "показать правила, но не менять holdfast.toml")
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
		fmt.Fprintln(a.out, "Правила:")
		for _, r := range accepted {
			fmt.Fprintf(a.out, "  %-13s %-30s  %s (%s)\n", r.Kind, r.Pattern, r.Source, r.Origin)
		}
	}
	if len(pending) > 0 {
		fmt.Fprintln(a.out, "\nНайдены возможные запреты в тексте (эвристика):")
		for _, r := range pending {
			fmt.Fprintf(a.out, "  %s  «%s»\n    → %s %s\n", r.Source(), r.Text, r.Kind, r.Pattern)
			if dryRun {
				continue
			}
			if yes || a.confirm("    Добавить в политику?") {
				accepted = append(accepted, r.ToSynced())
			}
		}
	}

	if dryRun {
		fmt.Fprintln(a.out, "\n--dry-run: holdfast.toml не изменён")
		return nil
	}
	if len(accepted) == 0 && len(cfg.Guard.Synced.Rules) == 0 {
		fmt.Fprintln(a.out, "Запретов для синхронизации не найдено")
		return nil
	}
	if err := config.WriteSynced(root, accepted); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "\nЗаписано правил: %d → %s [guard.synced]\n", len(accepted), config.FileName)
	return nil
}
