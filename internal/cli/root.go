// Package cli defines the holdfast commands with cobra.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/lint"
	// A blank import "_": the package is needed only for its init(), which
	// registers the rules HF001–HF009 in the lint registry.
	_ "github.com/roma2246/holdfast/internal/lint/rules"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitFindings = 1 // lint found errors
	ExitFailure  = 2 // the tool itself failed
)

// exitError is an error that carries an exit code. Its message is not printed.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// app is the state shared by all commands: I/O streams and flags.
type app struct {
	version string
	in      *bufio.Reader
	out     io.Writer
	errOut  io.Writer
	dir     string // -C / --dir
}

// Execute runs the CLI and returns the process exit code.
func Execute(version string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{version: version, in: bufio.NewReader(stdin), out: stdout, errOut: stderr}
	root := a.rootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)

	err := root.Execute()
	var ee exitError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ee): // errors.As looks through the chain for an error of the given type
		return ee.code
	default:
		fmt.Fprintln(stderr, "holdfast: error:", err)
		return ExitFailure
	}
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "holdfast",
		Short: "Instruction linter and guardrails for AI coding agents",
		Long: `Holdfast makes the work of AI agents (Claude Code, Cursor, Codex) in a
repository predictable and safe:

  holdfast lint    checks CLAUDE.md / AGENTS.md / .cursorrules
  holdfast guard   controls what the agent does with files and the terminal
  holdfast sync    turns the prohibitions in CLAUDE.md into guard policies
  holdfast doctor  checks that the hooks are connected and restore points are created`,
		Version:       a.version,
		SilenceUsage:  true, // don't print the usage on every error
		SilenceErrors: true, // we print errors ourselves in Execute
	}
	root.PersistentFlags().StringVarP(&a.dir, "dir", "C", ".", "repository directory")
	root.AddCommand(a.lintCmd(), a.initCmd(), a.syncCmd(), a.guardCmd(), a.doctorCmd())
	return root
}

// repoRoot is the repository root for --dir (the directory with .git or holdfast.toml).
func (a *app) repoRoot() (string, error) {
	abs, err := filepath.Abs(a.dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	return lint.FindRoot(abs), nil
}

// confirm asks a yes/no question. The default is no.
func (a *app) confirm(question string) bool {
	fmt.Fprintf(a.out, "%s [y/N]: ", question)
	line, _ := a.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "д", "да":
		return true
	}
	return false
}

// isTerminal reports whether w is connected to a terminal (for colored output).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}
