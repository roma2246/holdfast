package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roma2246/holdfast/internal/guard"
)

// newGitRepo is a git repository with one commit.
func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	return dir
}

// fakeBin is an executable file that serves as the "holdfast binary" for the hooks.
func fakeBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "holdfast")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDoctor(t *testing.T) {
	dir := newGitRepo(t)

	// Nothing is installed: no hooks and no restore points, so exit code 2 and hints.
	code, out, _ := run(t, "", "-C", dir, "doctor")
	if code != ExitFailure {
		t.Fatalf("exit code %d:\n%s", code, out)
	}
	for _, want := range []string{"✓ git is installed", "✗ Claude Code hooks", "holdfast guard install",
		"✓ holdfast.toml is readable", "✗ restore points", "holdfast guard start", "Problems:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}

	// Hooks with an explicit path to the binary and one restore point: all good.
	bin := fakeBin(t)
	if code, _, errOut := run(t, "", "-C", dir, "guard", "install", "--bin", bin); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := run(t, "", "-C", dir, "guard", "start"); code != 0 {
		t.Fatal(errOut)
	}
	code, out, _ = run(t, "", "-C", dir, "doctor")
	if code != 0 || strings.Contains(out, "✗") || !strings.Contains(out, "All good") ||
		!strings.Contains(out, "binary for the hooks: "+bin) || !strings.Contains(out, "1, the latest") {
		t.Fatalf("exit code %d:\n%s", code, out)
	}

	// The binary is gone.
	os.Remove(bin)
	if code, out, _ := run(t, "", "-C", dir, "doctor"); code != ExitFailure || !strings.Contains(out, bin+" does not exist") {
		t.Fatalf("%d\n%s", code, out)
	}
	if code, _, _ := run(t, "", "-C", dir, "guard", "uninstall"); code != 0 {
		t.Fatal(code)
	}
	if _, _, err := guard.InstallHook(dir, false, fakeBin(t)); err != nil {
		t.Fatal(err)
	}

	// A broken holdfast.toml and a recent config-error in the log.
	toml := filepath.Join(dir, "holdfast.toml")
	os.WriteFile(toml, []byte("[guard\n"), 0o644)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(toml, past, past)
	guard.AppendLog(dir, guard.LogEntry{Time: time.Now().UTC(), Tool: "Bash", Decision: "ask", Rule: "config-error", Reason: "holdfast.toml cannot be read"})
	guard.AppendLog(dir, guard.LogEntry{Time: past.Add(-time.Minute).UTC(), Tool: "Bash", Decision: "ask", Rule: "config-error", Reason: "before the edit"})
	code, out, _ = run(t, "", "-C", dir, "doctor")
	if code != ExitFailure || !strings.Contains(out, "✗ holdfast.toml is readable") ||
		!strings.Contains(out, "✗ no recent config-error entries in the log — 1 since the last change") {
		t.Fatalf("%d\n%s", code, out)
	}
	// The config is fixed (the file changed after the error), so the logged error is no longer recent.
	os.WriteFile(toml, []byte("[guard]\n"), 0o644)
	future := time.Now().Add(time.Minute)
	os.Chtimes(toml, future, future)
	if code, out, _ := run(t, "", "-C", dir, "doctor"); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}

	// A failed restore point.
	guard.AppendLog(dir, guard.LogEntry{Time: time.Now().UTC(), Tool: "UserPromptSubmit", Decision: "snapshot", Rule: "error", Reason: "disk full"})
	os.WriteFile(filepath.Join(dir, guard.StateDir, "checkpoint-error"), []byte("2026-11-15T10:00:00Z disk full\n"), 0o644)
	code, out, _ = run(t, "", "-C", dir, "doctor")
	if code != ExitFailure || !strings.Contains(out, "✗ restore points are created without errors — 2026-11-15T10:00:00Z disk full") {
		t.Fatalf("%d\n%s", code, out)
	}
	os.Remove(filepath.Join(dir, guard.StateDir, "checkpoint-error"))
	if _, out, _ := run(t, "", "-C", dir, "doctor"); !strings.Contains(out, "failed restore points in the last 24 h: 1") {
		t.Fatal(out)
	}
}

func TestDoctorBrokenSettingsAndNoGit(t *testing.T) {
	dir := t.TempDir() // not a git repository
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte("{oops"), 0o644)
	os.MkdirAll(filepath.Join(dir, guard.StateDir, "log.jsonl"), 0o755) // the log can't be read
	code, out, _ := run(t, "", "-C", dir, "doctor")
	for _, want := range []string{"invalid JSON", "✗ the log", "the directory is not in a git repository", "git init"} {
		if code != ExitFailure || !strings.Contains(out, want) {
			t.Fatalf("missing %q (exit code %d):\n%s", want, code, out)
		}
	}

	// git not found: the restore-point check honestly fails too.
	t.Setenv("PATH", t.TempDir())
	_, out, _ = run(t, "", "-C", dir, "doctor")
	for _, want := range []string{"✗ git is installed", "there are no restore points without git", "holdfast not found in PATH", "guard install --bin"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestResolveBin(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin", "dir"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", "holdfast"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", "plain"), []byte("x"), 0o644)
	tests := []struct {
		bin  string
		want string // a substring of the result or of the error
		ok   bool
	}{
		{"./bin/holdfast", filepath.Join(root, "bin", "holdfast"), true},
		{filepath.Join(root, "bin", "holdfast"), filepath.Join(root, "bin", "holdfast"), true},
		{"./bin/plain", "is not an executable file", false},
		{"./bin/dir", "is not an executable file", false},
		{"./bin/none", "does not exist", false},
		{"sh", "/sh", true},
		{"holdfast-no-such-binary", "not found in PATH", false},
	}
	for _, tt := range tests {
		got, err := resolveBin(root, tt.bin)
		msg := got
		if err != nil {
			msg = err.Error()
		}
		if (err == nil) != tt.ok || !strings.Contains(msg, tt.want) {
			t.Errorf("%q = %q, %v; want %q ok=%v", tt.bin, got, err, tt.want, tt.ok)
		}
	}
	if h := binHint("/x/holdfast"); !strings.Contains(h, "--bin /path") {
		t.Error(h)
	}
}

func TestGuardUninstall(t *testing.T) {
	dir := newGitRepo(t)
	settings := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	os.WriteFile(settings, []byte(`{"model":"opus"}`), 0o644)

	run(t, "", "-C", dir, "guard", "install")
	run(t, "", "-C", dir, "guard", "install", "--local")
	code, out, _ := run(t, "", "-C", dir, "guard", "uninstall")
	if code != 0 || !strings.Contains(out, "removed from") || !strings.Contains(out, "uninstall --local") ||
		!strings.Contains(out, "--purge") {
		t.Fatalf("%d\n%s", code, out)
	}
	data, _ := os.ReadFile(settings)
	var s map[string]any
	if json.Unmarshal(data, &s) != nil || len(s) != 1 || s["model"] != "opus" {
		t.Fatalf("%s", data)
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "uninstall"); !strings.Contains(out, "No holdfast hooks") {
		t.Fatal(out)
	}

	// --purge: declined, so nothing is deleted.
	run(t, "", "-C", dir, "guard", "start")
	_, out, _ = run(t, "n\n", "-C", dir, "guard", "uninstall", "--local", "--purge")
	if !strings.Contains(out, "Cancelled") || !strings.Contains(out, "restore points and backups: 1") || strings.Contains(out, "remain") {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(dir, guard.StateDir)); err != nil {
		t.Fatal(".holdfast was deleted without confirmation")
	}
	// Confirmed: the log and the refs are deleted.
	code, out, _ = run(t, "да\n", "-C", dir, "guard", "uninstall", "--purge")
	if code != 0 || !strings.Contains(out, "Deleted the log and all Holdfast refs (1)") {
		t.Fatalf("%d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, guard.StateDir)); err == nil {
		t.Fatal(".holdfast was not deleted")
	}
	if g, _ := guard.OpenGit(dir); g != nil {
		if refs, _ := g.AllRefs(); len(refs) != 0 {
			t.Fatalf("refs remain: %v", refs)
		}
	}
	if _, out, _ := run(t, "", "-C", dir, "guard", "uninstall", "--purge", "--yes"); !strings.Contains(out, "nothing to delete") {
		t.Fatal(out)
	}

	// Not a git repository: --purge deletes only .holdfast/.
	plain := t.TempDir()
	run(t, "", "-C", plain, "guard", "install")
	code, out, _ = run(t, "", "-C", plain, "guard", "uninstall", "--purge", "-y")
	if code != 0 || !strings.Contains(out, "all Holdfast refs (0)") {
		t.Fatalf("%d\n%s", code, out)
	}
}
