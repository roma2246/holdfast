package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/x/term"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/guard"
)

func (a *app) guardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "Предохранитель: хук Claude Code, точки восстановления, журнал и откат",
	}
	cmd.AddCommand(a.guardInstallCmd(), a.guardCheckCmd(), a.guardCheckpointCmd(), a.guardStartCmd(),
		a.guardListCmd(), a.guardDiffCmd(), a.guardRollbackCmd(), a.guardLogCmd())
	return cmd
}

func (a *app) guardInstallCmd() *cobra.Command {
	var (
		local bool
		bin   string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Подключить Holdfast к Claude Code (.claude/settings.json проекта)",
		Long: `Прописывает в .claude/settings.json два хука, не затирая другие настройки:
  PreToolUse        holdfast guard check       — проверка каждого действия агента
  UserPromptSubmit  holdfast guard checkpoint  — точка восстановления перед каждым запросом`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			path, changed, err := guard.InstallHook(root, local, bin)
			if err != nil {
				return err
			}
			if err := guard.EnsureStateDir(root); err != nil {
				return err
			}
			if !changed {
				fmt.Fprintf(a.out, "Хуки уже установлены в %s\n", path)
				return nil
			}
			fmt.Fprintf(a.out, "Хуки добавлены в %s:\n", path)
			fmt.Fprintf(a.out, "  • проверка действий агента (%s)\n", guard.HookMatcher)
			fmt.Fprintln(a.out, "  • точка восстановления перед каждым вашим запросом")
			fmt.Fprintln(a.out, "Убедитесь, что holdfast доступен в PATH, и перезапустите сессию Claude Code.")
			fmt.Fprintln(a.out, "Точки: holdfast guard list · откат: holdfast guard rollback")
			return nil
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "писать в .claude/settings.local.json (не коммитится)")
	cmd.Flags().StringVar(&bin, "bin", guard.DefaultBin, "как Claude Code вызывает holdfast (имя или полный путь)")
	return cmd
}

func (a *app) guardCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Проверить действие агента (вызывается хуком PreToolUse: JSON в stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := guard.RunHook(a.in, a.out, guard.DefaultHookEnv()); code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
}

func (a *app) guardCheckpointCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "checkpoint",
		Short: "Точка восстановления перед запросом (вызывается хуком UserPromptSubmit)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// При успехе ничего не печатаем (вывод этого хука попал бы в
			// контекст агента) и никогда не возвращаем ошибку: она помешала
			// бы запросу. О неудаче хук сам сообщает пользователю.
			guard.RunCheckpoint(a.in, a.out, guard.DefaultHookEnv())
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
		Short: "Сделать снимок рабочего дерева вручную",
		Long: `Сохраняет текущее состояние, включая неотслеживаемые файлы.
С подключёнными хуками (holdfast guard install) точки создаются автоматически
перед каждым запросом, и start нужен только для дополнительной точки.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			g, err := guard.OpenGit(root)
			if err != nil {
				return err
			}
			// .holdfast/.gitignore должен появиться до снапшота, иначе
			// журнал попадёт в снапшот как неотслеживаемый файл.
			if err := guard.EnsureStateDir(g.Dir); err != nil {
				return err
			}
			snap, err := g.CreateSnapshot(time.Now(), guard.SnapshotMeta{Kind: guard.KindManual})
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Снапшот %s сохранён (%s)\n", snap.Name(), snap.Ref)
			fmt.Fprintln(a.out, "Изменения: holdfast guard diff · откат: holdfast guard rollback")
			return nil
		},
	}
}

// pickSnapshot: --to N (1 — последняя точка) или --snapshot имя, иначе последняя.
func pickSnapshot(g *guard.Git, name string, to int) (guard.Snapshot, error) {
	if to > 0 {
		return g.SnapshotAt(to)
	}
	return g.FindSnapshot(name)
}

func (a *app) guardListCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Показать точки восстановления: когда, перед каким запросом, что изменилось с тех пор",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			g, err := guard.OpenGit(root)
			if err != nil {
				return err
			}
			snaps, err := g.Snapshots()
			if err != nil {
				return err
			}
			if msg := guard.LastCheckpointError(root); msg != "" {
				fmt.Fprintf(a.out, "⚠ Последняя автоматическая точка НЕ создана: %s\n  Изменения последнего запроса этой точкой не защищены.\n\n", msg)
			}
			if len(snaps) == 0 {
				fmt.Fprintln(a.out, "Точек восстановления пока нет.")
				fmt.Fprintln(a.out, "Подключите хуки (holdfast guard install) или сделайте снимок вручную: holdfast guard start")
				return nil
			}
			if limit > 0 && len(snaps) > limit {
				snaps = snaps[:limit]
			}
			cur, err := g.WriteTree()
			if err != nil {
				return err
			}
			now := time.Now()
			for i, s := range snaps {
				label := snapshotLabel(s)
				changes := "без изменений с тех пор"
				if s.Tree != cur {
					if ch, err := g.ChangesTo(s, cur); err == nil {
						changes = fmt.Sprintf("с тех пор: изменено %d, добавлено %d", len(ch.Modified), len(ch.Added))
					}
				}
				fmt.Fprintf(a.out, "%3d  %s (%s)  %s\n     %s\n", i+1,
					s.Time.Local().Format("01-02 15:04"), humanAgo(now.Sub(s.Time)), label, changes)
			}
			fmt.Fprintln(a.out, "\nОткат к точке: holdfast guard rollback --to <номер> · изменения: holdfast guard diff --to <номер>")
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 10, "сколько последних точек показать (0 — все)")
	return cmd
}

// snapshotLabel — как точка появилась.
func snapshotLabel(s guard.Snapshot) string {
	switch s.Meta.Kind {
	case guard.KindCheckpoint:
		if s.Meta.Prompt != "" {
			return "перед запросом «" + truncate(s.Meta.Prompt, 60) + "»"
		}
		return "перед запросом"
	case guard.KindPreDelete:
		return "перед массовым удалением: " + truncate(s.Meta.Prompt, 50)
	case guard.KindBackup:
		return "резервная копия " + truncate(s.Meta.Prompt, 50)
	}
	return "вручную"
}

// humanAgo — «5 мин назад», «3 ч назад», «2 дн назад».
func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	default:
		return fmt.Sprintf("%d дн назад", int(d.Hours()/24))
	}
}

func (a *app) guardDiffCmd() *cobra.Command {
	var (
		stat bool
		name string
		to   int
	)
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Показать изменения с момента точки восстановления",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := a.openGit()
			if err != nil {
				return err
			}
			snap, err := pickSnapshot(g, name, to)
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
	cmd.Flags().IntVar(&to, "to", 0, "номер точки из holdfast guard list (1 — последняя)")
	cmd.Flags().StringVar(&name, "snapshot", "", "имя снапшота или резервной копии (по умолчанию последний)")
	return cmd
}

func (a *app) guardRollbackCmd() *cobra.Command {
	var (
		yes  bool
		name string
		to   int
	)
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Вернуть файлы к точке восстановления (с подтверждением)",
		Long: `Возвращает рабочее дерево к точке восстановления: изменённые и удалённые
файлы восстанавливаются, появившиеся после точки — удаляются. Ветки, коммиты
и индекс git не меняются. Текущее состояние перед откатом сохраняется, так что
откат тоже можно отменить.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := a.openGit()
			if err != nil {
				return err
			}
			snap, err := pickSnapshot(g, name, to)
			if err != nil {
				return err
			}
			// План строится до любых изменений: всё, что откат перезапишет или
			// удалит, должно попасть в резервную копию — иначе откат не начнётся.
			plan, err := g.PlanRollback(snap)
			if err != nil {
				return err
			}
			ch := plan.Changes
			if ch.Empty() {
				fmt.Fprintf(a.out, "Рабочее дерево совпадает со снапшотом %s — откатывать нечего\n", snap.Name())
				return nil
			}
			fmt.Fprintf(a.out, "Откат к снапшоту %s (%s), %s:\n", snap.Name(),
				snap.Time.Local().Format("2006-01-02 15:04:05"), snapshotLabel(snap))
			hidden := map[string]bool{}
			for _, h := range plan.Hidden {
				hidden[h] = true
			}
			note := func(f string) string {
				for h := range hidden {
					if f == h || strings.HasPrefix(f, h+"/") {
						return "  (сейчас игнорируется git; текущая версия сохранится в резервной копии)"
					}
				}
				return ""
			}
			for _, f := range ch.Modified {
				fmt.Fprintf(a.out, "  восстановить  %s%s\n", f, note(f))
			}
			for _, f := range ch.Added {
				fmt.Fprintf(a.out, "  удалить       %s%s\n", f, note(f))
			}
			for _, f := range plan.Skipped {
				fmt.Fprintf(a.out, "  не трогать    %s  (вложенный git-репозиторий: его содержимое нельзя сохранить)\n", f)
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
			fmt.Fprintf(a.out, "Состояние до отката сохранено. Отменить откат: holdfast guard rollback --snapshot %s\n", backup.Name())
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "не спрашивать подтверждение")
	cmd.Flags().IntVar(&to, "to", 0, "номер точки из holdfast guard list (1 — последняя)")
	cmd.Flags().StringVar(&name, "snapshot", "", "имя снапшота или резервной копии (по умолчанию последний)")
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
			a.printLogTable(root, filtered)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "сколько последних записей показать (0 — все)")
	cmd.Flags().StringVar(&decision, "decision", "", "фильтр: allow, ask или deny")
	cmd.Flags().BoolVar(&asJSON, "json", false, "вывод в JSONL")
	return cmd
}

func (a *app) printLogTable(root string, entries []guard.LogEntry) {
	targetW, whyW := logColumnWidths(terminalWidth(a.out))
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
		t.Row(e.Time.Local().Format("01-02 15:04:05"), e.Decision, e.Tool,
			truncate(relTarget(root, e.Target), targetW), truncate(why, whyW))
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

// logColumnWidths делит ширину терминала между колонками «действие» и
// «причина»; остальные колонки узкие и фиксированные. Без терминала
// (вывод в файл или pipe) ширина не ограничена разумными значениями.
func logColumnWidths(termWidth int) (target, why int) {
	if termWidth <= 0 {
		return 48, 60
	}
	// время 14 + решение 7 + инструмент 12 + отступы 5×2 + рамки 6
	const fixed = 14 + 7 + 12 + 5*2 + 6
	rest := max(termWidth-fixed-1, 24)
	target = rest * 2 / 5
	return target, rest - target
}

// terminalWidth — ширина терминала, 0 — если w не терминал.
func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return 0
	}
	width, _, err := term.GetSize(f.Fd())
	if err != nil {
		return 0
	}
	return width
}

// relTarget показывает путь внутри проекта относительно его корня.
func relTarget(root, target string) string {
	if !filepath.IsAbs(target) {
		return target
	}
	if rel, err := filepath.Rel(root, target); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return target
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
