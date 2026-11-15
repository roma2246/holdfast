package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/config"
	"github.com/roma2246/holdfast/internal/guard"
)

// check is the result of one doctor check.
type check struct {
	ok     bool
	title  string // what was checked
	detail string // what was found
	hint   string // how to fix it (only for ✗)
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that Holdfast is connected and working",
		Long: `Checks the installation and prints ✓ or ✗ with a hint on how to fix it:
  git is installed; the hooks are registered in .claude/settings.json or
  settings.local.json; Claude Code will find the holdfast binary; holdfast.toml
  is readable; the log has no recent errors; restore points exist.

Exit code: 0 if everything is fine, 2 if there are problems.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			checks := runDoctor(root, time.Now())
			a.printChecks(root, checks)
			for _, c := range checks {
				if !c.ok {
					return exitError{ExitFailure}
				}
			}
			return nil
		},
	}
}

// runDoctor runs all the checks in order.
func runDoctor(root string, now time.Time) []check {
	gitOK, gitCheck := checkGitInstalled()
	hooks, hooksCheck := checkHooks(root)
	checks := []check{gitCheck, hooksCheck}
	checks = append(checks, checkBinaries(root, hooks)...)
	checks = append(checks, checkConfig(root))
	checks = append(checks, checkLog(root, now)...)
	return append(checks, checkSnapshots(root, gitOK, now))
}

func (a *app) printChecks(root string, checks []check) {
	r := lipgloss.NewRenderer(a.out)
	okMark := r.NewStyle().Foreground(lipgloss.Color("10")).Render("✓")
	badMark := r.NewStyle().Foreground(lipgloss.Color("9")).Bold(true).Render("✗")
	faint := r.NewStyle().Faint(true)

	fmt.Fprintf(a.out, "holdfast doctor — %s\n\n", root)
	problems := 0
	for _, c := range checks {
		mark := okMark
		if !c.ok {
			mark = badMark
			problems++
		}
		fmt.Fprintf(a.out, "%s %s", mark, c.title)
		if c.detail != "" {
			fmt.Fprint(a.out, faint.Render(" — "+c.detail))
		}
		fmt.Fprintln(a.out)
		if !c.ok && c.hint != "" {
			fmt.Fprintf(a.out, "  → %s\n", c.hint)
		}
	}
	fmt.Fprintln(a.out)
	if problems == 0 {
		fmt.Fprintln(a.out, "All good: the agent's actions are checked and restore points are being created.")
		return
	}
	fmt.Fprintf(a.out, "Problems: %d\n", problems)
}

func checkGitInstalled() (bool, check) {
	c := check{title: "git is installed"}
	path, err := exec.LookPath("git")
	if err != nil {
		c.detail = "git not found in PATH"
		c.hint = "install git: restore points and rollback work through it"
		return false, c
	}
	c.ok = true
	c.detail = path
	if out, err := exec.Command(path, "--version").Output(); err == nil {
		c.detail = strings.TrimSpace(string(out))
	}
	return true, c
}

// checkHooks looks for both holdfast hooks in the Claude Code settings.
func checkHooks(root string) ([]guard.InstalledHook, check) {
	c := check{title: "Claude Code hooks"}
	hooks, err := guard.FindHooks(root)
	if err != nil {
		c.detail = err.Error()
		c.hint = "fix the JSON in the file: until it can be read, Claude Code won't call the hooks"
		return hooks, c
	}
	files := map[string]bool{}
	have := map[string]bool{}
	for _, h := range hooks {
		have[h.Event] = true
		files[relTarget(root, h.File)] = true
	}
	var missing []string
	if !have["PreToolUse"] {
		missing = append(missing, "PreToolUse → holdfast guard check (checks the agent's actions)")
	}
	if !have["UserPromptSubmit"] {
		missing = append(missing, "UserPromptSubmit → holdfast guard checkpoint (restore points)")
	}
	if len(missing) > 0 {
		c.detail = "missing " + strings.Join(missing, "; ")
		c.hint = "holdfast guard install (or --local to keep the settings out of commits), then restart the Claude Code session"
		return hooks, c
	}
	c.ok = true
	c.detail = "PreToolUse and UserPromptSubmit in " + strings.Join(sortedKeys(files), ", ")
	return hooks, c
}

// checkBinaries verifies that Claude Code will find the program from every hook.
// With no hooks we check holdfast in PATH: that is what guard install would register.
func checkBinaries(root string, hooks []guard.InstalledHook) []check {
	bins := map[string]bool{}
	for _, h := range hooks {
		bins[h.Bin] = true
	}
	if len(bins) == 0 {
		bins[guard.DefaultBin] = true
	}
	var out []check
	for _, bin := range sortedKeys(bins) {
		c := check{title: "binary for the hooks: " + bin}
		path, err := resolveBin(root, bin)
		if err != nil {
			c.detail = err.Error()
			c.hint = binHint(bin)
		} else {
			c.ok, c.detail = true, path
		}
		out = append(out, c)
	}
	return out
}

// resolveBin finds the program the same way as the shell in which Claude Code
// runs the hook: a name without "/" is looked up in PATH, a path is taken
// relative to the project directory (Claude Code runs hooks from there).
func resolveBin(root, bin string) (string, error) {
	if !strings.Contains(bin, "/") {
		path, err := exec.LookPath(bin)
		if err != nil {
			return "", fmt.Errorf("%s not found in PATH", bin)
		}
		return path, nil
	}
	path := bin
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s does not exist", path)
	case fi.IsDir() || fi.Mode().Perm()&0o111 == 0: // not a single "x" bit
		return "", fmt.Errorf("%s is not an executable file", path)
	}
	return path, nil
}

func binHint(bin string) string {
	self, err := os.Executable()
	if err != nil || strings.Contains(bin, "/") {
		return "reinstall holdfast or register the hooks again with the right path: holdfast guard uninstall && holdfast guard install --bin /path/to/holdfast"
	}
	return fmt.Sprintf("add the directory with holdfast to PATH or register the full path: holdfast guard uninstall && holdfast guard install --bin %s", self)
}

func checkConfig(root string) check {
	c := check{title: config.FileName + " is readable"}
	if _, err := os.Stat(filepath.Join(root, config.FileName)); errors.Is(err, fs.ErrNotExist) {
		c.ok, c.detail = true, "no such file, the built-in rules apply"
		return c
	}
	if _, err := config.Load(root); err != nil {
		c.detail = err.Error()
		c.hint = "fix the file: until it can be read, the project rules are not applied and the agent's actions need confirmation"
		return c
	}
	c.ok = true
	return c
}

// recentWindow is which errors in the log count as recent.
const recentWindow = 24 * time.Hour

// checkLog looks in the log for errors that have not been fixed yet:
//   - a config-error after the last change to holdfast.toml (and no older
//     than a day): if the file was edited since, the error is most likely gone;
//   - a failed latest restore point (.holdfast/checkpoint-error is removed
//     by the very first successful restore point).
func checkLog(root string, now time.Time) []check {
	entries, err := guard.ReadLog(root)
	if err != nil {
		return []check{{title: "the log .holdfast/log.jsonl", detail: err.Error(),
			hint: "check the permissions on the .holdfast/ directory"}}
	}
	dayAgo := now.Add(-recentWindow)
	// A config-error is recent if it was logged after the last edit of
	// holdfast.toml. With no file, the config can't be unreadable.
	var cfgChanged time.Time
	cfgExists := false
	if fi, err := os.Stat(filepath.Join(root, config.FileName)); err == nil {
		cfgExists, cfgChanged = true, fi.ModTime()
	}
	var (
		cfgErrors, snapErrors, checkpoints, total int
		lastCfgErr                                string
	)
	for _, e := range entries {
		if !e.Time.After(dayAgo) {
			continue
		}
		total++
		switch {
		case e.Rule == "config-error":
			if cfgExists && e.Time.After(cfgChanged) {
				cfgErrors++
				lastCfgErr = e.Reason
			}
		case e.Decision == "snapshot" && e.Rule == "error":
			snapErrors++
		case e.Decision == "snapshot":
			checkpoints++
		}
	}

	logCheck := check{title: "no recent config-error entries in the log", ok: cfgErrors == 0}
	if cfgErrors > 0 {
		logCheck.detail = fmt.Sprintf("%d since the last change to %s: %s", cfgErrors, config.FileName, truncate(lastCfgErr, 120))
		logCheck.hint = "fix " + config.FileName + " and check: holdfast guard log --decision ask"
	} else {
		logCheck.detail = fmt.Sprintf("entries in the last 24 h: %d", total)
	}

	cpCheck := check{title: "restore points are created without errors", ok: true}
	if msg := guard.LastCheckpointError(root); msg != "" {
		cpCheck.ok = false
		cpCheck.detail = truncate(msg, 160)
		cpCheck.hint = "fix the cause; the next restore point will be created before the next prompt (or manually: holdfast guard start)"
	} else if snapErrors > 0 {
		cpCheck.detail = fmt.Sprintf("failed restore points in the last 24 h: %d, later ones were created fine", snapErrors)
	} else if checkpoints > 0 {
		cpCheck.detail = fmt.Sprintf("in the last 24 h: %d", checkpoints)
	} else {
		cpCheck.detail = "no errors"
	}
	return []check{logCheck, cpCheck}
}

func checkSnapshots(root string, gitOK bool, now time.Time) check {
	c := check{title: "restore points in " + guard.SnapshotRefPrefix}
	if !gitOK {
		c.detail = "there are no restore points without git"
		c.hint = "install git"
		return c
	}
	g, err := guard.OpenGit(root)
	if err != nil {
		c.detail = "the directory is not in a git repository"
		c.hint = "git init: restore points are stored in git"
		return c
	}
	snaps, err := g.Snapshots()
	if err != nil {
		c.detail = err.Error()
		return c
	}
	if len(snaps) == 0 {
		c.detail = "none yet"
		c.hint = "a restore point will appear before the next prompt to Claude Code (the hooks are required); manually: holdfast guard start"
		return c
	}
	c.ok = true
	c.detail = fmt.Sprintf("%d, the latest %s", len(snaps), humanAgo(now.Sub(snaps[0].Time)))
	return c
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out) // sorts in place; the slices package is available since Go 1.21

	return out
}
