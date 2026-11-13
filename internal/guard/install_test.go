package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallHookPreservesSettings(t *testing.T) {
	root := t.TempDir()
	existing := `{
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "other-hook"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]
  }
}`
	write(t, root, ".claude/settings.json", existing)

	path, changed, err := InstallHook(root, false, DefaultHookCommand)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var s map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s["permissions"] == nil {
		t.Error("permissions потеряны")
	}
	hooks := s["hooks"].(map[string]any)
	if hooks["Stop"] == nil {
		t.Error("хук Stop потерян")
	}
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("PreToolUse: %v", pre)
	}
	last := pre[1].(map[string]any)
	if last["matcher"] != HookMatcher {
		t.Errorf("matcher = %v", last["matcher"])
	}

	// Повторная установка ничего не меняет.
	if _, changed, err := InstallHook(root, false, DefaultHookCommand); err != nil || changed {
		t.Fatalf("повторная установка: changed=%v err=%v", changed, err)
	}
}

func TestInstallHookNewAndLocal(t *testing.T) {
	root := t.TempDir()
	path, changed, err := InstallHook(root, true, "/usr/local/bin/holdfast guard check")
	if err != nil || !changed || filepath.Base(path) != "settings.local.json" {
		t.Fatalf("%s %v %v", path, changed, err)
	}
	write(t, root, ".claude/settings.json", "{broken")
	if _, _, err := InstallHook(root, false, DefaultHookCommand); err == nil {
		t.Fatal("ожидалась ошибка для битого JSON")
	}
	write(t, root, ".claude/settings.json", "  ")
	if _, changed, err := InstallHook(root, false, DefaultHookCommand); err != nil || !changed {
		t.Fatalf("пустой файл: %v %v", changed, err)
	}
}

func TestReadLogMissingAndCorrupt(t *testing.T) {
	root := t.TempDir()
	if e, err := ReadLog(root); err != nil || e != nil {
		t.Fatalf("%v %v", e, err)
	}
	write(t, root, ".holdfast/log.jsonl", "{\"tool\":\"Bash\",\"decision\":\"deny\"}\nnot-json\n")
	e, err := ReadLog(root)
	if err != nil || len(e) != 1 || e[0].Decision != "deny" {
		t.Fatalf("%+v %v", e, err)
	}
}
