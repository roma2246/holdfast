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
		Short: "Guardrails: the Claude Code hook, restore points, the log and rollback",
	}
	cmd.AddCommand(a.guardInstallCmd(), a.guardUninstallCmd(), a.guardCheckCmd(), a.guardCheckpointCmd(), a.guardStartCmd(),
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
		Short: "Connect Holdfast to Claude Code (the project's .claude/settings.json)",
		Long: `Adds two hooks to .claude/settings.json without overwriting other settings:
  PreToolUse        holdfast guard check       checks every action of the agent
  UserPromptSubmit  holdfast guard checkpoint  saves a restore point before every prompt`,
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
				fmt.Fprintf(a.out, "Hooks are already installed in %s\n", path)
				return nil
			}
			fmt.Fprintf(a.out, "Hooks added to %s:\n", path)
			fmt.Fprintf(a.out, "  • checking the agent's actions (%s)\n", guard.HookMatcher)
			fmt.Fprintln(a.out, "  • a restore point before every prompt you send")
			fmt.Fprintln(a.out, "Make sure holdfast is on your PATH and restart the Claude Code session.")
			fmt.Fprintln(a.out, "Restore points: holdfast guard list · rollback: holdfast guard rollback")
			return nil
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "write to .claude/settings.local.json (not committed)")
	cmd.Flags().StringVar(&bin, "bin", guard.DefaultBin, "how Claude Code invokes holdfast (a name or a full path)")
	return cmd
}

func (a *app) guardUninstallCmd() *cobra.Command {
	var local, purge, yes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Disconnect Holdfast from Claude Code (the inverse of guard install)",
		Long: `Removes only the holdfast hooks from .claude/settings.json (or from
settings.local.json with --local); other settings and other tools' hooks stay as they were.

Restore points and the log are kept by default: rollback keeps working.
--purge, after a confirmation, deletes those too: the .holdfast/ directory and
all refs/holdfast*/ refs in every working tree of the repository.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			path, removed, err := guard.UninstallHook(root, local)
			if err != nil {
				return err
			}
			if removed > 0 {
				fmt.Fprintf(a.out, "holdfast hooks removed from %s (%d). Restart the Claude Code session.\n", path, removed)
			} else {
				fmt.Fprintf(a.out, "No holdfast hooks in %s\n", path)
			}
			// Hooks may remain in the second file: tell the user how to remove those too.
			if hooks, _ := guard.FindHooks(root); len(hooks) > 0 {
				flag := " --local"
				if local {
					flag = ""
				}
				fmt.Fprintf(a.out, "holdfast hooks remain in %s: holdfast guard uninstall%s\n", hooks[0].File, flag)
			}
			if !purge {
				fmt.Fprintln(a.out, "Restore points and the log are kept, rollback still works: holdfast guard list.")
				fmt.Fprintln(a.out, "To delete them too: holdfast guard uninstall --purge")
				return nil
			}
			return a.purge(root, yes)
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "remove the hooks from .claude/settings.local.json")
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete .holdfast/ and all restore points")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation for --purge")
	return cmd
}

// purge deletes the log and all restore points after a confirmation.
func (a *app) purge(root string, yes bool) error {
	stateDir := filepath.Join(root, guard.StateDir)
	_, statErr := os.Stat(stateDir)
	var refs []string
	g, gitErr := guard.OpenGit(root)
	if gitErr == nil {
		var err error
		if refs, err = g.AllRefs(); err != nil {
			return err
		}
	}
	if statErr != nil && len(refs) == 0 {
		fmt.Fprintln(a.out, "There is no log and there are no restore points: nothing to delete")
		return nil
	}
	fmt.Fprintln(a.out, "This will be deleted permanently:")
	if statErr == nil {
		fmt.Fprintf(a.out, "  %s  (the log and service files)\n", stateDir)
	}
	if len(refs) > 0 {
		fmt.Fprintf(a.out, "  restore points and backups: %d (refs/holdfast*/, all working trees)\n", len(refs))
		fmt.Fprintln(a.out, "  You will not be able to roll back to them.")
	}
	if !yes && !a.confirm("Delete?") {
		fmt.Fprintln(a.out, "Cancelled")
		return nil
	}
	if err := os.RemoveAll(stateDir); err != nil {
		return err
	}
	n := 0
	if gitErr == nil {
		var err error
		if n, err = g.PurgeRefs(); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.out, "Deleted the log and all Holdfast refs (%d). git's regular garbage collection (git gc) will remove the objects.\n", n)
	return nil
}

func (a *app) guardCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check an agent action (called by the PreToolUse hook: JSON on stdin)",
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
		Short: "Save a restore point before a prompt (called by the UserPromptSubmit hook)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// On success nothing is printed (this hook's output would end up in
			// the agent's context) and an error is never returned: it would get
			// in the way of the prompt. The hook reports a failure to the user itself.
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
		Short: "Take a snapshot of the working tree manually",
		Long: `Saves the current state, including untracked files.
With the hooks connected (holdfast guard install) restore points are created
automatically before every prompt, and start is only needed for an extra one.`,
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
			// .holdfast/.gitignore has to exist before the snapshot, otherwise
			// the log would get into the snapshot as an untracked file.
			if err := guard.EnsureStateDir(g.Dir); err != nil {
				return err
			}
			snap, err := g.CreateSnapshot(time.Now(), guard.SnapshotMeta{Kind: guard.KindManual})
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Snapshot %s saved (%s)\n", snap.Name(), snap.Ref)
			fmt.Fprintln(a.out, "Changes: holdfast guard diff · rollback: holdfast guard rollback")
			return nil
		},
	}
}

// pickSnapshot: --to N (1 is the latest restore point) or --snapshot name, otherwise the latest.
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
		Short: "Show restore points: when, before which prompt, what has changed since",
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
				fmt.Fprintf(a.out, "⚠ The latest automatic restore point was NOT created: %s\n  The changes from the last prompt are not covered by a restore point.\n\n", msg)
			}
			if len(snaps) == 0 {
				fmt.Fprintln(a.out, "No restore points yet.")
				fmt.Fprintln(a.out, "Connect the hooks (holdfast guard install) or take a snapshot manually: holdfast guard start")
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
				changes := "no changes since"
				if s.Tree != cur {
					if ch, err := g.ChangesTo(s, cur); err == nil {
						changes = fmt.Sprintf("since then: %d changed, %d added", len(ch.Modified), len(ch.Added))
					}
				}
				fmt.Fprintf(a.out, "%3d  %s (%s)  %s\n     %s\n", i+1,
					s.Time.Local().Format("01-02 15:04"), humanAgo(now.Sub(s.Time)), label, changes)
			}
			fmt.Fprintln(a.out, "\nRoll back to a point: holdfast guard rollback --to <number> · changes: holdfast guard diff --to <number>")
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 10, "how many of the latest restore points to show (0 for all)")
	return cmd
}

// snapshotLabel is how the restore point came about.
func snapshotLabel(s guard.Snapshot) string {
	switch s.Meta.Kind {
	case guard.KindCheckpoint:
		if s.Meta.Prompt != "" {
			return "before prompt \"" + truncate(s.Meta.Prompt, 60) + "\""
		}
		return "before prompt"
	case guard.KindPreDelete:
		return "before mass deletion: " + truncate(s.Meta.Prompt, 50)
	case guard.KindBackup:
		return "backup " + truncate(s.Meta.Prompt, 50)
	}
	return "manual"
}

// humanAgo gives "5 min ago", "3 h ago", "2 days ago".
func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func (a *app) guardDiffCmd() *cobra.Command {
	var (
		stat     bool
		name     string
		to       int
		pathFlag []string
	)
	cmd := &cobra.Command{
		Use:   "diff [PATH...]",
		Short: "Show the changes since a restore point",
		Long: `Shows what has changed since a restore point.
If PATH arguments or --path flags are given, only changes matching those paths
are shown.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := a.openGit()
			if err != nil {
				return err
			}
			snap, err := pickSnapshot(g, name, to)
			if err != nil {
				return err
			}
			rawPaths := append(args, pathFlag...)
			filterPaths := a.normalizeFilterPaths(g.Dir, rawPaths)
			ch, err := g.ChangesSince(snap, filterPaths...)
			if err != nil {
				return err
			}
			if ch.Empty() {
				if len(filterPaths) > 0 {
					fmt.Fprintf(a.out, "No changes matching the specified path(s) since snapshot %s\n", snap.Name())
				} else {
					fmt.Fprintf(a.out, "No changes since snapshot %s\n", snap.Name())
				}
				return nil
			}
			return g.Diff(a.out, snap, guard.DiffOptions{Stat: stat, Color: isTerminal(a.out)}, filterPaths...)
		},
	}
	cmd.Flags().BoolVar(&stat, "stat", false, "per-file statistics only")
	cmd.Flags().IntVar(&to, "to", 0, "restore point number from holdfast guard list (1 is the latest)")
	cmd.Flags().StringVar(&name, "snapshot", "", "name of a snapshot or backup (the latest by default)")
	cmd.Flags().StringSliceVarP(&pathFlag, "path", "p", nil, "specific file or glob to show diff for")
	return cmd
}

func (a *app) guardRollbackCmd() *cobra.Command {
	var (
		yes      bool
		name     string
		to       int
		pathFlag []string
	)
	cmd := &cobra.Command{
		Use:   "rollback [PATH...]",
		Short: "Return the files to a restore point (asks for confirmation)",
		Long: `Returns the working tree to a restore point: changed and deleted files are
restored, files that appeared after the point are deleted. Branches, commits
and the git index are not changed. The current state is saved before the
rollback, so the rollback itself can be undone.

If PATH arguments or --path flags are given, only matching files or directories
are rolled back (selective rollback); other files are left as they are.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := a.openGit()
			if err != nil {
				return err
			}
			snap, err := pickSnapshot(g, name, to)
			if err != nil {
				return err
			}
			rawPaths := append(args, pathFlag...)
			filterPaths := a.normalizeFilterPaths(g.Dir, rawPaths)

			// The plan is built before any changes: everything the rollback would
			// overwrite or delete has to be in the backup, otherwise the rollback won't start.
			plan, err := g.PlanRollback(snap, filterPaths...)
			if err != nil {
				return err
			}
			ch := plan.Changes
			if ch.Empty() {
				if len(filterPaths) > 0 {
					fmt.Fprintf(a.out, "No changes matching the specified path(s) since snapshot %s\n", snap.Name())
				} else {
					fmt.Fprintf(a.out, "The working tree matches snapshot %s: nothing to roll back\n", snap.Name())
				}
				return nil
			}
			targetDesc := ""
			if len(filterPaths) > 0 {
				total := len(ch.Modified) + len(ch.Added)
				targetDesc = fmt.Sprintf(" (%d matching file%s)", total, plural(total))
			}
			fmt.Fprintf(a.out, "Rollback%s to snapshot %s (%s), %s:\n", targetDesc, snap.Name(),
				snap.Time.Local().Format("2006-01-02 15:04:05"), snapshotLabel(snap))
			hidden := map[string]bool{}
			for _, h := range plan.Hidden {
				hidden[h] = true
			}
			note := func(f string) string {
				for h := range hidden {
					if f == h || strings.HasPrefix(f, h+"/") {
						return "  (currently ignored by git; the current version will be saved in the backup)"
					}
				}
				return ""
			}
			for _, f := range ch.Modified {
				fmt.Fprintf(a.out, "  restore  %s%s\n", f, note(f))
			}
			for _, f := range ch.Added {
				fmt.Fprintf(a.out, "  delete   %s%s\n", f, note(f))
			}
			for _, f := range plan.Skipped {
				fmt.Fprintf(a.out, "  keep     %s  (a nested git repository: its contents can't be saved)\n", f)
			}
			if !yes && !a.confirm("Roll back these changes?") {
				fmt.Fprintln(a.out, "Cancelled")
				return nil
			}
			done, backup, err := g.Rollback(snap, time.Now(), filterPaths...)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Done: %d restored, %d deleted.\n", len(done.Modified), len(done.Added))
			fmt.Fprintf(a.out, "The state before the rollback is saved. To undo the rollback: holdfast guard rollback --snapshot %s\n", backup.Name())
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	cmd.Flags().IntVar(&to, "to", 0, "restore point number from holdfast guard list (1 is the latest)")
	cmd.Flags().StringVar(&name, "snapshot", "", "name of a snapshot or backup (the latest by default)")
	cmd.Flags().StringSliceVarP(&pathFlag, "path", "p", nil, "specific file or glob to roll back (can be repeated)")
	return cmd
}

// normalizeFilterPaths converts user-supplied path patterns into slash-separated paths relative to root.
func (a *app) normalizeFilterPaths(root string, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	baseDir := a.dir
	if !filepath.IsAbs(baseDir) {
		if abs, err := filepath.Abs(baseDir); err == nil {
			baseDir = abs
		}
	}
	var out []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.ContainsAny(p, "*?[") {
			if filepath.IsAbs(p) {
				if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
					out = append(out, filepath.ToSlash(rel))
					continue
				}
			} else if baseDir != root && isWithinPath(root, baseDir) {
				if relDir, err := filepath.Rel(root, baseDir); err == nil && !strings.HasPrefix(relDir, "..") {
					out = append(out, filepath.ToSlash(filepath.Join(relDir, p)))
					continue
				}
			}
			out = append(out, filepath.ToSlash(p))
			continue
		}
		abs := p
		if !filepath.IsAbs(p) {
			abs = filepath.Join(baseDir, p)
		}
		if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
			out = append(out, filepath.ToSlash(rel))
		} else {
			out = append(out, filepath.ToSlash(filepath.Clean(p)))
		}
	}
	return out
}

func isWithinPath(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (a *app) guardLogCmd() *cobra.Command {
	var (
		limit    int
		decision string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show the log: what was allowed, what was blocked, and why",
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
				filtered = filtered[len(filtered)-limit:] // the last limit entries
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
				fmt.Fprintln(a.out, "The log is empty")
				return nil
			}
			a.printLogTable(root, filtered)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "how many of the latest entries to show (0 for all)")
	cmd.Flags().StringVar(&decision, "decision", "", "filter: allow, ask or deny")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSONL")
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
		Headers("TIME", "DECISION", "TOOL", "ACTION", "RULE / REASON")
	for _, e := range entries {
		why := e.Rule
		if e.Decision != "allow" && e.Reason != "" {
			why = e.Rule + ": " + e.Reason
		}
		if len(e.Context) > 0 {
			why = strings.TrimPrefix(why+" · hint: "+strings.Join(e.Context, ", "), " · ")
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

// logColumnWidths splits the terminal width between the "action" and "reason"
// columns; the other columns are narrow and fixed. Without a terminal (output
// to a file or a pipe) the width is capped at reasonable values.
func logColumnWidths(termWidth int) (target, why int) {
	if termWidth <= 0 {
		return 48, 60
	}
	// time 14 + decision 7 + tool 12 + padding 5×2 + borders 6
	const fixed = 14 + 7 + 12 + 5*2 + 6
	rest := max(termWidth-fixed-1, 24)
	target = rest * 2 / 5
	return target, rest - target
}

// terminalWidth is the width of the terminal, 0 if w is not a terminal.
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

// relTarget shows a path inside the project relative to its root.
func relTarget(root, target string) string {
	if !filepath.IsAbs(target) {
		return target
	}
	if rel, err := filepath.Rel(root, target); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return target
}

// truncate cuts a string by characters (not bytes) and removes line breaks.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
