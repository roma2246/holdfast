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
		decision string // "" — хук ничего не выводит (allow)
	}{
		{"deny rm home", "Bash", map[string]any{"command": "ls && rm -rf ~"}, "deny"},
		{"ask push", "Bash", map[string]any{"command": "git push"}, "ask"},
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
				t.Fatalf("код выхода %d", code)
			}
			if tt.decision == "" {
				if out.Len() != 0 {
					t.Fatalf("при allow хук должен молчать, вывел: %s", out.String())
				}
				return
			}
			var resp hookOutput
			if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
				t.Fatalf("некорректный JSON %q: %v", out.String(), err)
			}
			h := resp.HookSpecificOutput
			if h.HookEventName != "PreToolUse" || h.PermissionDecision != tt.decision || !strings.HasPrefix(h.PermissionDecisionReason, "holdfast [") {
				t.Fatalf("ответ %+v, ожидалось решение %s", h, tt.decision)
			}
		})
	}

	entries, err := ReadLog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(tests) {
		t.Fatalf("в журнале %d записей, ожидалось %d", len(entries), len(tests))
	}
	if e := entries[0]; e.Tool != "Bash" || e.Decision != "deny" || e.Rule != "rm-home" || e.Target != "ls && rm -rf ~" || e.Session != "s1" {
		t.Errorf("первая запись журнала: %+v", e)
	}
	// .holdfast/.gitignore создан, чтобы журнал не попал в git.
	if _, err := os.Stat(filepath.Join(root, StateDir, ".gitignore")); err != nil {
		t.Error(err)
	}
}

func TestRunHookBadInput(t *testing.T) {
	root := t.TempDir()
	for _, in := range []string{"not json", `{"tool_name":"Bash","tool_input":"oops"}`} {
		var out bytes.Buffer
		if code := RunHook(strings.NewReader(in), &out, testEnv(root)); code != 0 {
			t.Fatalf("код %d", code)
		}
		if !strings.Contains(out.String(), `"permissionDecision":"ask"`) {
			t.Errorf("при плохом входе ожидался ask, получено %s", out.String())
		}
	}
}

func TestRunHookRecoversFromPanic(t *testing.T) {
	root := t.TempDir()
	orig := evaluate
	evaluate = func(string, HookInput) Verdict { panic("boom") }
	// t.Cleanup выполнится после теста — возвращаем настоящую функцию.
	t.Cleanup(func() { evaluate = orig })

	var out bytes.Buffer
	code := RunHook(bytes.NewReader(hookJSON(t, root, "Bash", map[string]any{"command": "ls"})), &out, testEnv(root))
	if code != 0 || !strings.Contains(out.String(), `"permissionDecision":"ask"`) || !strings.Contains(out.String(), "boom") {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	entries, _ := ReadLog(root)
	if len(entries) != 1 || entries[0].Rule != "internal-error" {
		t.Fatalf("паника должна попасть в журнал: %+v", entries)
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
		t.Fatalf("protected_paths из конфига не применён: %s", out.String())
	}
}

// Требование ТЗ: хук отвечает быстрее 10 мс.
func TestHookLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("короткий режим")
	}
	res := testing.Benchmark(BenchmarkHook)
	perOp := time.Duration(res.NsPerOp())
	t.Logf("хук: %v на вызов", perOp)
	if perOp > 10*time.Millisecond {
		t.Fatalf("хук слишком медленный: %v > 10ms", perOp)
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
