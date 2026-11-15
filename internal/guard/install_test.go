package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	path, changed, err := InstallHook(root, false, DefaultBin)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var s map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s["permissions"] == nil {
		t.Error("permissions were lost")
	}
	hooks := s["hooks"].(map[string]any)
	if hooks["Stop"] == nil {
		t.Error("the Stop hook was lost")
	}
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("PreToolUse: %v", pre)
	}
	last := pre[1].(map[string]any)
	if last["matcher"] != HookMatcher {
		t.Errorf("matcher = %v", last["matcher"])
	}
	ups := hooks["UserPromptSubmit"].([]any)
	cmd := ups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]
	if len(ups) != 1 || cmd != "holdfast guard checkpoint" {
		t.Errorf("UserPromptSubmit: %v", ups)
	}

	// Installing again changes nothing.
	if _, changed, err := InstallHook(root, false, DefaultBin); err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
}

func TestInstallHookNewAndLocal(t *testing.T) {
	root := t.TempDir()
	path, changed, err := InstallHook(root, true, "/usr/local/bin/holdfast")
	if err != nil || !changed || filepath.Base(path) != "settings.local.json" {
		t.Fatalf("%s %v %v", path, changed, err)
	}
	write(t, root, ".claude/settings.json", "{broken")
	if _, _, err := InstallHook(root, false, DefaultBin); err == nil {
		t.Fatal("expected an error for broken JSON")
	}
	write(t, root, ".claude/settings.json", "  ")
	if _, changed, err := InstallHook(root, false, DefaultBin); err != nil || !changed {
		t.Fatalf("empty file: %v %v", changed, err)
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

func TestInstallHookAddsMissingEvent(t *testing.T) {
	// An old install (PreToolUse only) is completed with the restore-point hook.
	root := t.TempDir()
	write(t, root, ".claude/settings.json", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"holdfast guard check"}]}]}}`)
	_, changed, err := InstallHook(root, false, "")
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	data, _ := os.ReadFile(filepath.Join(root, ".claude/settings.json"))
	if strings.Count(string(data), "guard check\"") != 1 || !strings.Contains(string(data), "guard checkpoint") {
		t.Fatalf("%s", data)
	}
}
