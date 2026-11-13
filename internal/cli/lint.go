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
		Short: "Проверить файлы инструкций агента (CLAUDE.md, AGENTS.md, .cursorrules)",
		Long: `Находит CLAUDE.md, AGENTS.md и .cursorrules в корне и вложенных папках
и проверяет их правилами HF001–HF009. Все проверки локальные, без LLM.

Коды выхода: 0 — нет ошибок, 1 — есть error (с --strict и warning), 2 — сбой.
Выключить правило: disable = ["HF007"] в holdfast.toml
или комментарием <!-- holdfast-disable HF003 --> в файле.`,
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
				return fmt.Errorf("неизвестный формат %q (human или json)", format)
			}
			path := a.dir
			if len(args) == 1 {
				path = args[0]
			}
			return a.runLint(cmd.Context(), path, format, strict)
		},
	}
	cmd.Flags().StringVarP(&format, "format", "f", "human", "формат вывода: human или json")
	cmd.Flags().BoolVar(&strict, "strict", false, "считать warning ошибкой")
	cmd.Flags().BoolVar(&listRules, "list-rules", false, "показать список правил")
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
			fmt.Fprintf(a.out, "Файлы инструкций (%v) не найдены в %s\n", cfg.Lint.Files, root)
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
