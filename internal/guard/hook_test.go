package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testEnv(root string) HookEnv {
	return HookEnv{
		Getenv: func(k string) string {
			if k == "CLAUDE_PROJECT_DIR" {
				return root
			}
			return ""
		},
		Getwd: func() (string, error) { return root, nil },
		Now:   func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) },
	}
}

func hookJSON(t testing.TB, root, tool string, input map[string]any) []byte {
	data, err := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": root, "hook_event_name": "PreToolUse",
		"tool_name": tool, "tool_input": input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRunHook(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name     string
		tool     string
		input    map[string]any
		decision string // "" means the hook prints nothing (allow)
	}{
		{"deny rm home", "Bash", map[string]any{"command": "ls && rm -rf ~"}, "deny"},
		{"push outside a git repository", "Bash", map[string]any{"command": "git push"}, ""}, // git will report the error itself
		{"allow ls", "Bash", map[string]any{"command": "ls"}, ""},
		{"deny edit env", "Edit", map[string]any{"file_path": filepath.Join(root, ".env")}, "deny"},
		{"deny write pem", "Write", map[string]any{"file_path": filepath.Join(root, "k.pem"), "content": "x"}, "deny"},
		{"deny read env", "Read", map[string]any{"file_path": filepath.Join(root, ".env")}, "deny"},
		{"deny notebook", "NotebookEdit", map[string]any{"notebook_path": filepath.Join(root, ".env")}, "deny"},
		{"allow multiedit", "MultiEdit", map[string]any{"file_path": filepath.Join(root, "a.go")}, ""},
		{"allow other tool", "Glob", map[string]any{"pattern": "**/*.go"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			code := RunHook(bytes.NewReader(hookJSON(t, root, tt.tool, tt.input)), &out, testEnv(root))
			if code != 0 {
				t.Fatalf("exit code %d", code)
			}
			if tt.decision == "" {
				if out.Len() != 0 {
					t.Fatalf("on allow the hook must stay silent, it printed: %s", out.String())
				}
				return
			}
			var resp hookOutput
			if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
				t.Fatalf("invalid JSON %q: %v", out.String(), err)
			}
			h := resp.HookSpecificOutput
			if h.HookEventName != "PreToolUse" || h.PermissionDecision != tt.decision || !strings.HasPrefix(h.PermissionDecisionReason, "holdfast [") {
				t.Fatalf("reply %+v, expected the decision %s", h, tt.decision)
			}
		})
	}

	entries, err := ReadLog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(tests) {
		t.Fatalf("the log has %d entries, expected %d", len(entries), len(tests))
	}
	if e := entries[0]; e.Tool != "Bash" || e.Decision != "deny" || e.Rule != "rm-home" || e.Target != "ls && rm -rf ~" || e.Session != "s1" {
		t.Errorf("first log entry: %+v", e)
	}
	// .holdfast/.gitignore is created so that the log does not get into git.
	if _, err := os.Stat(filepath.Join(root, StateDir, ".gitignore")); err != nil {
		t.Error(err)
	}
}

func TestRunHookBadInput(t *testing.T) {
	root := t.TempDir()
	for _, in := range []string{"not json", `{"tool_name":"Bash","tool_input":"oops"}`} {
		var out bytes.Buffer
		if code := RunHook(strings.NewReader(in), &out, testEnv(root)); code != 0 {
			t.Fatalf("exit code %d", code)
		}
		if !strings.Contains(out.String(), `"permissionDecision":"ask"`) {
			t.Errorf("expected ask on bad input, got %s", out.String())
		}
	}
}

func TestRunHookRecoversFromPanic(t *testing.T) {
	root := t.TempDir()
	orig := evaluate
	evaluate = func(string, HookInput) (Verdict, *Policy) { panic("boom") }
	// t.Cleanup runs after the test: we put the real function back.
	t.Cleanup(func() { evaluate = orig })

	var out bytes.Buffer
	code := RunHook(bytes.NewReader(hookJSON(t, root, "Bash", map[string]any{"command": "ls"})), &out, testEnv(root))
	if code != 0 || !strings.Contains(out.String(), `"permissionDecision":"ask"`) || !strings.Contains(out.String(), "boom") {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	entries, _ := ReadLog(root)
	if len(entries) != 1 || entries[0].Rule != "internal-error" {
		t.Fatalf("the panic must be logged: %+v", entries)
	}
}

func TestProjectRootFallbacks(t *testing.T) {
	env := HookEnv{Getenv: func(string) string { return "" }, Getwd: func() (string, error) { return "/wd", nil }}
	if got := projectRoot(HookInput{Cwd: "/cwd"}, env); got != "/cwd" {
		t.Errorf("root = %s", got)
	}
	if got := projectRoot(HookInput{}, env); got != "/wd" {
		t.Errorf("root = %s", got)
	}
}

func TestHookUsesConfig(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "holdfast.toml"), []byte("[guard]\nprotected_paths = [\"db/**\"]\n"), 0o644)
	var out bytes.Buffer
	RunHook(bytes.NewReader(hookJSON(t, root, "Write", map[string]any{"file_path": filepath.Join(root, "db/x.sql")})), &out, testEnv(root))
	if !strings.Contains(out.String(), `"ask"`) {
		t.Fatalf("protected_paths from the config was not applied: %s", out.String())
	}
}

// Requirement: the hook answers in under 10 ms.
func TestHookLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	res := testing.Benchmark(BenchmarkHook)
	perOp := time.Duration(res.NsPerOp())
	t.Logf("hook: %v per call", perOp)
	if perOp > 10*time.Millisecond {
		t.Fatalf("the hook is too slow: %v > 10ms", perOp)
	}
}

func BenchmarkHook(b *testing.B) {
	root := b.TempDir()
	os.WriteFile(filepath.Join(root, "holdfast.toml"), []byte("[guard]\nprotected_paths = [\"migrations/**\"]\nask_commands = [\"git push\"]\n"), 0o644)
	input := hookJSON(b, root, "Bash", map[string]any{
		"command": `cd src && go test ./... && bash -c "rm -rf build" | tee log.txt; git push origin main`,
	})
	env := testEnv(root)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		RunHook(bytes.NewReader(input), &bytes.Buffer{}, env)
	}
}

func TestHookContextNotes(t *testing.T) {
	root := t.TempDir()
	// The instruction text is deliberately in Russian: a user's CLAUDE.md and
	// notes can be in any language, and the hints must still be found.
	write(t, root, "CLAUDE.md", "# P\n\n## Rules\n\n- Never edit `migrations/` — они уже в продакшене.\n- Код API живёт в src/api/.\n\n```\nmigrations/ в блоке кода не считается\n```\n")
	write(t, root, "holdfast.toml", "[[guard.context]]\npaths = [\"migrations/**\"]\nnote = \"Добавляй новую миграцию 00N_описание.sql\"\n")
	write(t, root, "migrations/001.sql", "x")
	env := testEnv(root)

	call := func(session, tool string, input map[string]any) hookOutput {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"session_id": session, "cwd": root, "tool_name": tool, "tool_input": input})
		var out bytes.Buffer
		RunHook(bytes.NewReader(data), &out, env)
		var resp hookOutput
		if out.Len() > 0 {
			if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
				t.Fatalf("%v: %s", err, out.String())
			}
		}
		return resp
	}

	mig := map[string]any{"file_path": filepath.Join(root, "migrations/001.sql")}
	resp := call("sess-1", "Read", mig)
	h := resp.HookSpecificOutput
	if h.PermissionDecision != "" {
		t.Fatalf("a hint must not change the decision: %+v", h)
	}
	for _, want := range []string{"holdfast.toml", "00N_описание.sql", "CLAUDE.md:5", "Never edit"} {
		if !strings.Contains(h.AdditionalContext, want) {
			t.Errorf("the hint has no %q:\n%s", want, h.AdditionalContext)
		}
	}
	if strings.Contains(h.AdditionalContext, "блоке кода") || strings.Contains(h.AdditionalContext, "src/api") {
		t.Errorf("extra lines in the hint:\n%s", h.AdditionalContext)
	}

	// Again in the same session: not shown; in a new one: shown again.
	if again := call("sess-1", "Edit", mig); again.HookSpecificOutput.AdditionalContext != "" {
		t.Errorf("repeat in the same session: %+v", again)
	}
	if other := call("sess-2", "Bash", map[string]any{"command": "sed -i s/a/b/ migrations/001.sql"}); !strings.Contains(other.HookSpecificOutput.AdditionalContext, "Never edit") {
		t.Errorf("a new session and Bash must get the hint: %+v", other)
	}
	// A file without rules: the hook stays silent.
	if none := call("sess-3", "Read", map[string]any{"file_path": filepath.Join(root, "README.md")}); none.HookSpecificOutput.HookEventName != "" {
		t.Errorf("the hook must stay silent for a file without rules: %+v", none)
	}
	// The source of the hint is written to the log.
	entries, _ := ReadLog(root)
	if len(entries) == 0 || len(entries[0].Context) != 2 {
		t.Fatalf("log: %+v", entries)
	}
}

func TestFormatAndFilterNotes(t *testing.T) {
	if FormatNotes(nil) != "" {
		t.Error("empty list")
	}
	notes := []Note{{"a", "x"}, {"b", "y"}}
	if got := FilterSeen(t.TempDir(), "", notes); len(got) != 2 {
		t.Error("no filtering without a session_id")
	}
	root := t.TempDir()
	if got := FilterSeen(root, "../../evil/../id", notes); len(got) != 2 {
		t.Error("first showing")
	}
	if got := FilterSeen(root, "../../evil/../id", notes); len(got) != 0 {
		t.Error("the second showing must be filtered out")
	}
	if _, err := os.Stat(filepath.Join(root, StateDir, "seen", "evilid")); err != nil {
		t.Errorf("the session name must be cleaned of ../: %v", err)
	}
}
