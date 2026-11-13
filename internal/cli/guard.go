package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/guard"
)

func (a *app) guardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "Предохранитель: хук Claude Code, журнал, снапшоты и откат",
	}
	cmd.AddCommand(a.guardInstallCmd(), a.guardCheckCmd(), a.guardStartCmd(),
		a.guardDiffCmd(), a.guardRollbackCmd(), a.guardLogCmd())
	return cmd
}

func (a *app) guardInstallCmd() *cobra.Command {
	var (
		local   bool
		command string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Прописать PreToolUse-хук в .claude/settings.json проекта",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			path, changed, err := guard.InstallHook(root, local, command)
			if err != nil {
				return err
			}
			if err := guard.EnsureStateDir(root); err != nil {
				return err
			}
			if !changed {
				fmt.Fprintf(a.out, "Хук уже установлен в %s\n", path)
				return nil
			}
			fmt.Fprintf(a.out, "Хук добавлен в %s (инструменты: %s)\n", path, guard.HookMatcher)
			fmt.Fprintln(a.out, "Убедитесь, что holdfast доступен в PATH, и перезапустите сессию Claude Code.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "писать в .claude/settings.local.json (не коммитится)")
	cmd.Flags().StringVar(&command, "command", guard.DefaultHookCommand, "команда, которую запустит Claude Code")
	return cmd
}

func (a *app) guardCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Проверить действие агента (вызывается хуком: JSON в stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := guard.RunHook(a.in, a.out, guard.DefaultHookEnv()); code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
}

func (a *app) openGit() (*guard.Git, error) {
	root, err := a.repoRoot()
	if err != nil {
		return nil, err
	}
	return guard.OpenGit(root)
}

func (a *app) guardStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Сделать снапшот рабочего дерева перед сессией агента",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := a.openGit()
			if err != nil {
				return err
			}
			// .holdfast/.gitignore должен появиться до снапшота, иначе
			// журнал попадёт в снапшот как неотслеживаемый файл.
			if err := guard.EnsureStateDir(g.Dir); err != nil {
				return err
			}
			snap, err := g.CreateSnapshot(guard.SnapshotRefPrefix, time.Now())
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Снапшот %s сохранён (%s)\n", snap.Name(), snap.Ref)
			fmt.Fprintln(a.out, "Изменения: holdfast guard diff · откат: holdfast guard rollback")
			return nil
		},
	}
}

func (a *app) guardDiffCmd() *cobra.Command {
	var (
		stat bool
		name string
	)
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Показать изменения с момента снапшота",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := a.openGit()
			if err != nil {
				return err
			}
			snap, err := g.FindSnapshot(name)
			if err != nil {
				return err
			}
			ch, err := g.ChangesSince(snap)
			if err != nil {
				return err
			}
			if ch.Empty() {
				fmt.Fprintf(a.out, "Изменений с момента снапшота %s нет\n", snap.Name())
				return nil
			}
			return g.Diff(a.out, snap, guard.DiffOptions{Stat: stat, Color: isTerminal(a.out)})
		},
	}
	cmd.Flags().BoolVar(&stat, "stat", false, "только статистика по файлам")
	cmd.Flags().StringVar(&name, "snapshot", "", "имя снапшота (по умолчанию последний)")
	return cmd
}

func (a *app) guardRollbackCmd() *cobra.Command {
	var (
		yes  bool
		name string
	)
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Вернуть файлы к снапшоту (с подтверждением)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := a.openGit()
			if err != nil {
				return err
			}
			snap, err := g.FindSnapshot(name)
			if err != nil {
				return err
			}
			ch, err := g.ChangesSince(snap)
			if err != nil {
				return err
			}
			if ch.Empty() {
				fmt.Fprintf(a.out, "Рабочее дерево совпадает со снапшотом %s — откатывать нечего\n", snap.Name())
				return nil
			}
			fmt.Fprintf(a.out, "Откат к снапшоту %s (%s):\n", snap.Name(), snap.Time.Local().Format("2006-01-02 15:04:05"))
			for _, f := range ch.Modified {
				fmt.Fprintf(a.out, "  восстановить  %s\n", f)
			}
			for _, f := range ch.Added {
				fmt.Fprintf(a.out, "  удалить       %s\n", f)
			}
			if !yes && !a.confirm("Откатить эти изменения?") {
				fmt.Fprintln(a.out, "Отменено")
				return nil
			}
			done, backup, err := g.Rollback(snap, time.Now())
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Готово: восстановлено %d, удалено %d файлов.\n", len(done.Modified), len(done.Added))
			fmt.Fprintf(a.out, "Состояние до отката сохранено в %s\n", backup.Ref)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "не спрашивать подтверждение")
	cmd.Flags().StringVar(&name, "snapshot", "", "имя снапшота (по умолчанию последний)")
	return cmd
}

func (a *app) guardLogCmd() *cobra.Command {
	var (
		limit    int
		decision string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Показать журнал: что разрешено, что заблокировано и почему",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			entries, err := guard.ReadLog(root)
			if err != nil {
				return err
			}
			var filtered []guard.LogEntry
			for _, e := range entries {
				if decision == "" || e.Decision == decision {
					filtered = append(filtered, e)
				}
			}
			if limit > 0 && len(filtered) > limit {
				filtered = filtered[len(filtered)-limit:] // последние limit записей
			}
			if asJSON {
				enc := json.NewEncoder(a.out)
				for _, e := range filtered {
					if err := enc.Encode(e); err != nil {
						return err
					}
				}
				return nil
			}
			if len(filtered) == 0 {
				fmt.Fprintln(a.out, "Журнал пуст")
				return nil
			}
			a.printLogTable(filtered)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "сколько последних записей показать (0 — все)")
	cmd.Flags().StringVar(&decision, "decision", "", "фильтр: allow, ask или deny")
	cmd.Flags().BoolVar(&asJSON, "json", false, "вывод в JSONL")
	return cmd
}

func (a *app) printLogTable(entries []guard.LogEntry) {
	r := lipgloss.NewRenderer(a.out)
	colors := map[string]lipgloss.Style{
		"deny":  r.NewStyle().Foreground(lipgloss.Color("9")).Bold(true),
		"ask":   r.NewStyle().Foreground(lipgloss.Color("11")),
		"allow": r.NewStyle().Foreground(lipgloss.Color("10")),
	}
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(r.NewStyle().Faint(true)).
		Headers("ВРЕМЯ", "РЕШЕНИЕ", "ИНСТРУМЕНТ", "ДЕЙСТВИЕ", "ПРАВИЛО / ПРИЧИНА")
	for _, e := range entries {
		why := e.Rule
		if e.Decision != "allow" && e.Reason != "" {
			why = e.Rule + ": " + e.Reason
		}
		if len(e.Context) > 0 {
			why = strings.TrimPrefix(why+" · подсказка: "+strings.Join(e.Context, ", "), " · ")
		}
		t.Row(e.Time.Local().Format("01-02 15:04:05"), e.Decision, e.Tool, truncate(e.Target, 48), truncate(why, 60))
	}
	t.StyleFunc(func(row, col int) lipgloss.Style {
		s := r.NewStyle().Padding(0, 1)
		if row >= 0 && col == 1 {
			return colors[entries[row].Decision].Padding(0, 1)
		}
		return s
	})
	fmt.Fprintln(a.out, t.Render())
}

// truncate обрезает строку по символам (не байтам) и убирает переводы строк.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
