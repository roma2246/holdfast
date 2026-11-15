package cli

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roma2246/holdfast/internal/config"
)

const fixtures = "../../testdata/lint"

// run runs the CLI as if from a terminal and returns the exit code and the output.
func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Execute("test", args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

// copyDir copies a fixture into a temp directory (tests don't spoil testdata).
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestHelpAndVersion(t *testing.T) {
	code, out, _ := run(t, "", "--help")
	if code != 0 || !strings.Contains(out, "lint") || !strings.Contains(out, "guard") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out, _ := run(t, "", "--version"); code != 0 || !strings.Contains(out, "test") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _, errOut := run(t, "", "nope"); code != ExitFailure || errOut == "" {
		t.Fatalf("unknown command: %d %q", code, errOut)
	}
}

func TestLintExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"errors → 1", []string{"lint", filepath.Join(fixtures, "HF002", "bad")}, ExitFindings},
		{"clean → 0", []string{"lint", filepath.Join(fixtures, "HF008", "good")}, ExitOK},
		{"warning → 0", []string{"lint", filepath.Join(fixtures, "HF005", "bad")}, ExitOK},
		{"warning + --strict → 1", []string{"lint", "--strict", filepath.Join(fixtures, "HF005", "bad")}, ExitFindings},
		{"single file", []string{"lint", filepath.Join(fixtures, "HF006", "bad", "CLAUDE.md")}, ExitFindings},
		{"missing path → 2", []string{"lint", "/nonexistent/path"}, ExitFailure},
		{"bad format → 2", []string{"lint", "--format", "xml", fixtures}, ExitFailure},
		{"via -C", []string{"-C", filepath.Join(fixtures, "HF002", "bad"), "lint"}, ExitFindings},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, out, errOut := run(t, "", tt.args...); code != tt.want {
				t.Fatalf("exit code %d, want %d\n%s%s", code, tt.want, out, errOut)
			}
		})
	}
}

func TestLintHumanOutput(t *testing.T) {
	_, out, _ := run(t, "", "lint", filepath.Join(fixtures, "HF002", "bad"))
	// Line format: CLAUDE.md:14  HF002 error  "npm run e2e": ...
	if !strings.Contains(out, "CLAUDE.md:5  HF002 error    \"npm run e2e\": script e2e is not in package.json") {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(out, "errors: 3") {
		t.Fatalf("no summary:\n%s", out)
	}
	_, out, _ = run(t, "", "lint", filepath.Join(fixtures, "HF008", "good"))
	if !strings.Contains(out, "no problems found") {
		t.Fatal(out)
	}
	_, out, _ = run(t, "", "lint", t.TempDir())
	if !strings.Contains(out, "No instruction files") {
		t.Fatal(out)
	}
	_, out, _ = run(t, "", "lint", "--list-rules")
	if strings.Count(out, "HF0") != 9 {
		t.Fatal(out)
	}
}

func TestLintJSON(t *testing.T) {
	code, out, _ := run(t, "", "lint", "--format", "json", filepath.Join(fixtures, "all"))
	if code != ExitFindings {
		t.Fatalf("exit code %d", code)
	}
	var report struct {
		Findings []struct {
			Rule     string `json:"rule"`
			Severity string `json:"severity"`
			File     string `json:"file"`
			Line     int    `json:"line"`
		} `json:"findings"`
		Summary struct{ Files, Errors, Warnings, Infos int } `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if report.Summary.Files != 1 || report.Summary.Errors == 0 || report.Findings[0].Severity == "" {
		t.Fatalf("%+v", report)
	}
	_, out, _ = run(t, "", "lint", "-f", "json", t.TempDir())
	if !strings.Contains(out, `"findings": []`) {
		t.Fatal(out)
	}
}

func TestInitAndSync(t *testing.T) {
	dir := copyDir(t, filepath.Join(fixtures, "HF009", "bad"))
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(`# P

<!-- holdfast
deny-paths: secrets/**
-->

- Never edit `+"`migrations/`"+` by hand.
- Never run `+"`docker compose down`"+`.
`), 0o644)

	if code, out, _ := run(t, "", "-C", dir, "init"); code != 0 || !strings.Contains(out, "holdfast.toml") {
		t.Fatalf("init: %d %s", code, out)
	}
	if code, _, _ := run(t, "", "-C", dir, "init"); code != ExitFailure {
		t.Fatal("a second init must fail")
	}

	// Before sync, lint warns with HF009 (exit code 1 with --strict).
	if code, out, _ := run(t, "", "lint", "--strict", dir); code != ExitFindings || !strings.Contains(out, "HF009") {
		t.Fatalf("lint before sync: %d\n%s", code, out)
	}

	// --dry-run writes nothing.
	run(t, "", "-C", dir, "sync", "--dry-run")
	cfg, _ := config.Load(dir)
	if len(cfg.Guard.Synced.Rules) != 0 {
		t.Fatal("dry-run changed the config")
	}

	// Confirm the first heuristic rule and decline the second.
	code, out, _ := run(t, "y\nn\n", "-C", dir, "sync")
	if code != 0 || !strings.Contains(out, "Add to the policy?") {
		t.Fatalf("sync: %d\n%s", code, out)
	}
	cfg, _ = config.Load(dir)
	got := []string{}
	for _, r := range cfg.Guard.Synced.Rules {
		got = append(got, r.Kind+" "+r.Pattern+" "+r.Source)
	}
	want := "deny-path secrets/** CLAUDE.md:4|deny-path migrations/** CLAUDE.md:7"
	if strings.Join(got, "|") != want {
		t.Fatalf("synced:\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}

	// A repeated sync remembers the confirmed rule; --yes accepts the rest.
	if code, _, _ := run(t, "", "-C", dir, "sync", "--yes"); code != 0 {
		t.Fatal(code)
	}
	cfg, _ = config.Load(dir)
	if len(cfg.Guard.Synced.Rules) != 3 {
		t.Fatalf("%+v", cfg.Guard.Synced.Rules)
	}
	// After sync there are no HF009 warnings.
	if _, out, _ := run(t, "", "lint", dir); strings.Contains(out, "HF009") {
		t.Fatalf("HF009 after sync:\n%s", out)
	}
	// --no-heuristics keeps only the explicit block.
	run(t, "", "-C", dir, "sync", "--no-heuristics")
	cfg, _ = config.Load(dir)
	if len(cfg.Guard.Synced.Rules) != 1 {
		t.Fatalf("%+v", cfg.Guard.Synced.Rules)
	}

	empty := t.TempDir()
	if _, out, _ := run(t, "", "-C", empty, "sync"); !strings.Contains(out, "No prohibitions to sync") {
		t.Fatal(out)
	}
}

func TestGuardInstallCheckLog(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := run(t, "", "-C", dir, "guard", "install")
	if code != 0 || !strings.Contains(out, "Hooks added") {
		t.Fatalf("%d %s", code, out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "install"); !strings.Contains(out, "already installed") {
		t.Fatal(out)
	}

	if _, out, _ := run(t, "", "-C", dir, "guard", "log"); !strings.Contains(out, "The log is empty") {
		t.Fatal(out)
	}

	t.Setenv("CLAUDE_PROJECT_DIR", dir) // t.Setenv restores the variable after the test
	for _, cmd := range []string{"rm -rf ~", "ls", "git push"} {
		in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]string{"command": cmd}})
		code, out, _ := run(t, string(in), "guard", "check")
		if code != 0 {
			t.Fatalf("check %q: exit code %d", cmd, code)
		}
		if cmd == "rm -rf ~" && !strings.Contains(out, `"deny"`) {
			t.Fatalf("check %q: %s", cmd, out)
		}
	}

	_, out, _ = run(t, "", "-C", dir, "guard", "log")
	for _, want := range []string{"deny", "rm-home", "allow", "git push", "RULE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the log has no %q:\n%s", want, out)
		}
	}
	// The hint from CLAUDE.md is visible in the log.
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("- Never edit `db/` by hand.\n"), 0o644)
	in, _ := json.Marshal(map[string]any{"tool_name": "Read", "cwd": dir, "tool_input": map[string]string{"file_path": filepath.Join(dir, "db", "x.sql")}})
	if _, out, _ := run(t, string(in), "guard", "check"); !strings.Contains(out, "additionalContext") {
		t.Fatalf("no hint: %s", out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "log"); !strings.Contains(out, "hint: CLAUDE.md:1") {
		t.Fatalf("the hint is missing from the log:\n%s", out)
	}
	_, out, _ = run(t, "", "-C", dir, "guard", "log", "--decision", "allow", "--json")
	if strings.Count(out, "\n") != 3 || !strings.Contains(out, `"ls"`) || !strings.Contains(out, `"context":["CLAUDE.md:1"]`) {
		t.Fatal(out)
	}
	_, out, _ = run(t, "", "-C", dir, "guard", "log", "-n", "1", "--json")
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "x.sql") {
		t.Fatal(out)
	}
}

func TestGuardSnapshotCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	gitCmd := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	gitCmd("init", "-q")
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	gitCmd("add", "-A")
	gitCmd("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")

	if code, _, errOut := run(t, "", "-C", dir, "guard", "diff"); code != ExitFailure || !strings.Contains(errOut, "guard start") {
		t.Fatalf("diff without a snapshot: %d %s", code, errOut)
	}
	if code, out, _ := run(t, "", "-C", dir, "guard", "start"); code != 0 || !strings.Contains(out, "Snapshot") {
		t.Fatalf("start: %d %s", code, out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "diff"); !strings.Contains(out, "No changes") {
		t.Fatal(out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "rollback"); !strings.Contains(out, "nothing to roll back") {
		t.Fatal(out)
	}

	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // broken by agent\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("junk\n"), 0o644)

	if _, out, _ := run(t, "", "-C", dir, "guard", "diff"); !strings.Contains(out, "broken by agent") || !strings.Contains(out, "junk.txt") {
		t.Fatal(out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "diff", "--stat"); !strings.Contains(out, "2 files changed") {
		t.Fatal(out)
	}
	// Declining the confirmation changes nothing.
	if _, out, _ := run(t, "n\n", "-C", dir, "guard", "rollback"); !strings.Contains(out, "Cancelled") {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); err != nil {
		t.Fatal("the file must stay after declining")
	}
	code, out, _ := run(t, "y\n", "-C", dir, "guard", "rollback")
	if code != 0 || !strings.Contains(out, "1 restored, 1 deleted") {
		t.Fatalf("rollback: %d %s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(data) != "package main\n" {
		t.Fatalf("main.go = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Fatal("junk.txt must be deleted")
	}
	// Restore points: the checkpoint hook prints nothing; list and --to.
	// After the rollback the tree matches the manual snapshot, so a new restore
	// point appears only if something has changed.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("my edit before the prompt\n"), 0o644)
	cp := `{"session_id":"s1","cwd":"` + dir + `","prompt":"break it and fix it"}`
	if code, out, _ := run(t, cp, "guard", "checkpoint"); code != 0 || out != "" {
		t.Fatalf("checkpoint must stay silent: %d %q", code, out)
	}
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // v2\n"), 0o644)
	if _, out, _ := run(t, "", "-C", dir, "guard", "list"); !strings.Contains(out, "before prompt \"break it and fix it\"") || !strings.Contains(out, "1 changed") {
		t.Fatalf("list:\n%s", out)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "diff", "--to", "1", "--stat"); !strings.Contains(out, "main.go") {
		t.Fatalf("diff --to:\n%s", out)
	}
	if code, out, _ := run(t, "", "-C", dir, "guard", "rollback", "--to", "1", "--yes"); code != 0 || !strings.Contains(out, "break it and fix it") {
		t.Fatalf("rollback --to: %d\n%s", code, out)
	}
	if code, _, errOut := run(t, "", "-C", dir, "guard", "rollback", "--to", "99"); code != ExitFailure || !strings.Contains(errOut, "guard list") {
		t.Fatalf("wrong number: %d %s", code, errOut)
	}
	if code, _, _ := run(t, "", "-C", dir, "guard", "rollback", "--yes", "--snapshot", "missing"); code != ExitFailure {
		t.Fatal("an unknown snapshot must produce an error")
	}
	if code, _, _ := run(t, "", "-C", t.TempDir(), "guard", "start"); code != ExitFailure {
		t.Fatal("start outside git must fail")
	}
}

func TestGuardListEmptyAndHumanAgo(t *testing.T) {
	dir := t.TempDir()
	c := exec.Command("git", "init", "-q")
	c.Dir = dir
	if err := c.Run(); err != nil {
		t.Skip("git is not available")
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "list"); !strings.Contains(out, "No restore points yet") {
		t.Fatal(out)
	}
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 5 * time.Minute: "5 min ago",
		3 * time.Hour: "3 h ago", 72 * time.Hour: "3 days ago",
	} {
		if got := humanAgo(d); got != want {
			t.Errorf("humanAgo(%v) = %q", d, got)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("привет\nмир", 20); got != "привет ⏎ мир" {
		t.Error(got)
	}
	if got := truncate("абвгдеёжз", 5); got != "абвг…" {
		t.Error(got)
	}
}

func TestLogColumnWidths(t *testing.T) {
	if tw, ww := logColumnWidths(0); tw != 48 || ww != 60 {
		t.Errorf("without a terminal: %d %d", tw, ww)
	}
	for _, width := range []int{80, 120, 200} {
		tw, ww := logColumnWidths(width)
		if total := 14 + 7 + 12 + 5*2 + 6 + tw + ww; total > width {
			t.Errorf("width %d: the table (%d) does not fit", width, total)
		}
	}
	if tw, ww := logColumnWidths(30); tw+ww != 24 {
		t.Errorf("narrow terminal: minimum 24, got %d", tw+ww)
	}
	if got := relTarget("/repo", "/repo/src/a.go"); got != "src/a.go" {
		t.Error(got)
	}
	if got := relTarget("/repo", "/etc/hosts"); got != "/etc/hosts" {
		t.Error(got)
	}
	if got := relTarget("/repo", "ls -la"); got != "ls -la" {
		t.Error(got)
	}
}
