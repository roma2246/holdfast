package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseHoldfastHook(t *testing.T) {
	tests := []struct {
		cmd, bin, sub string
		ok            bool
	}{
		{"holdfast guard check", "holdfast", "check", true},
		{"holdfast guard checkpoint", "holdfast", "checkpoint", true},
		{"/usr/local/bin/holdfast guard check", "/usr/local/bin/holdfast", "check", true},
		{"'/opt/my tools/holdfast' guard checkpoint", "/opt/my tools/holdfast", "checkpoint", true},
		{"holdfast-dev guard check", "holdfast-dev", "check", true},
		{"other-hook", "", "", false},
		{"other guard check", "", "", false},        // чужая программа
		{"holdfast guard list", "", "", false},      // не хук
		{"holdfast guard check --x", "", "", false}, // не наш формат
		{"$HF guard check", "", "", false},
		{"X=1 holdfast guard check", "", "", false},
		{"holdfast guard check > /tmp/log", "", "", false},
		{"holdfast guard check; rm -rf /", "", "", false},
		{"holdfast 'guard", "", "", false}, // не разбирается
		{"(holdfast guard check)", "", "", false},
	}
	for _, tt := range tests {
		bin, sub, ok := parseHoldfastHook(tt.cmd)
		if bin != tt.bin || sub != tt.sub || ok != tt.ok {
			t.Errorf("%q = %q %q %v, want %q %q %v", tt.cmd, bin, sub, ok, tt.bin, tt.sub, tt.ok)
		}
	}
}

func TestUninstallHookRestoresSettings(t *testing.T) {
	root := t.TempDir()
	original := `{
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "other-hook"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]
  }
}`
	write(t, root, ".claude/settings.json", original)
	var want map[string]any
	if err := json.Unmarshal([]byte(original), &want); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstallHook(root, false, "/opt/my tools/holdfast"); err != nil {
		t.Fatal(err)
	}
	path, removed, err := UninstallHook(root, false)
	if err != nil || removed != 2 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	// install + uninstall = исходные настройки (с точностью до форматирования).
	if got := readJSON(t, path); !reflect.DeepEqual(got, want) {
		t.Fatalf("настройки изменились:\n got %v\nwant %v", got, want)
	}
	// Повторный uninstall ничего не меняет.
	if _, removed, err := UninstallHook(root, false); err != nil || removed != 0 {
		t.Fatalf("повторно: %d %v", removed, err)
	}
}

func TestUninstallHookMixedGroupAndEmpty(t *testing.T) {
	root := t.TempDir()
	// Наш хук в одной группе с чужим: чужой остаётся, группа сохраняется.
	write(t, root, ".claude/settings.local.json", `{"hooks":{
  "PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"holdfast guard check"},{"type":"command","command":"lint-hook"}]}, "junk"],
  "UserPromptSubmit":[{"hooks":[{"type":"command","command":"holdfast guard checkpoint"}]}],
  "Weird": "not-a-list"
}}`)
	path, removed, err := UninstallHook(root, true)
	if err != nil || removed != 2 {
		t.Fatalf("%d %v", removed, err)
	}
	got := readJSON(t, path)
	hooks := got["hooks"].(map[string]any)
	if _, ok := hooks["UserPromptSubmit"]; ok {
		t.Fatalf("пустое событие не удалено: %v", hooks)
	}
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 || !strings.Contains(mustJSON(pre), "lint-hook") || strings.Contains(mustJSON(pre), "holdfast") {
		t.Fatalf("PreToolUse: %v", pre)
	}
	if hooks["Weird"] != "not-a-list" {
		t.Fatalf("чужой ключ изменён: %v", hooks)
	}

	// Только хуки holdfast: ключ "hooks" исчезает целиком.
	write(t, root, ".claude/settings.json", `{"model":"opus","hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"holdfast guard check"}]}]}}`)
	path, _, err = UninstallHook(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := readJSON(t, path); !reflect.DeepEqual(got, map[string]any{"model": "opus"}) {
		t.Fatalf("%v", got)
	}

	// Файла нет — ничего не делаем и не создаём.
	empty := t.TempDir()
	if _, removed, err := UninstallHook(empty, false); err != nil || removed != 0 {
		t.Fatalf("%d %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(empty, ".claude")); err == nil {
		t.Fatal("uninstall создал .claude/")
	}
	// Битый JSON — ошибка, файл не трогаем.
	write(t, empty, ".claude/settings.json", "{oops")
	if _, _, err := UninstallHook(empty, false); err == nil {
		t.Fatal("ожидалась ошибка JSON")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestFindHooks(t *testing.T) {
	root := t.TempDir()
	if hooks, err := FindHooks(root); err != nil || len(hooks) != 0 {
		t.Fatalf("%v %v", hooks, err)
	}
	// Подкоманда должна соответствовать событию: checkpoint в PreToolUse не считается.
	write(t, root, ".claude/settings.json", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"holdfast guard checkpoint"}]}]}}`)
	if hooks, err := FindHooks(root); err != nil || len(hooks) != 0 {
		t.Fatalf("%v %v", hooks, err)
	}
	if _, _, err := InstallHook(root, true, "./bin/holdfast"); err != nil {
		t.Fatal(err)
	}
	hooks, err := FindHooks(root)
	if err != nil || len(hooks) != 2 {
		t.Fatalf("%+v %v", hooks, err)
	}
	for _, h := range hooks {
		if h.Bin != "./bin/holdfast" || !strings.HasSuffix(h.File, "settings.local.json") {
			t.Fatalf("%+v", h)
		}
	}
	write(t, root, ".claude/settings.json", "[1,2]")
	if hooks, err := FindHooks(root); err == nil || len(hooks) != 2 {
		t.Fatalf("битый settings.json: %v %v", hooks, err)
	}
}

func TestLogRotation(t *testing.T) {
	root := t.TempDir()
	entry := func(i int) LogEntry {
		return LogEntry{Time: time.Unix(int64(i), 0).UTC(), Tool: "Bash", Target: strings.Repeat("x", 40), Decision: "allow", Rule: string(rune('a' + i))}
	}
	// Порог 100 байт: каждая запись ~120 байт, ротация перед каждой второй записью.
	for i := range 3 {
		if err := appendLog(root, entry(i), 100); err != nil {
			t.Fatal(err)
		}
	}
	// После трёх записей: log.1 = запись 1 (запись 0 ушла при второй ротации), log = запись 2.
	got, err := ReadLog(root)
	if err != nil || len(got) != 2 || got[0].Rule != "b" || got[1].Rule != "c" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, StateDir, "log.lock")); err == nil {
		t.Fatal("замок не удалён")
	}

	// Свежий замок (ротирует другой процесс) — ротации нет, запись есть.
	lock := filepath.Join(root, StateDir, "log.lock")
	write(t, root, filepath.Join(StateDir, "log.lock"), "")
	if err := appendLog(root, entry(3), 100); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadLog(root); len(got) != 3 {
		t.Fatalf("при чужом замке запись потеряна: %+v", got)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("чужой свежий замок удалён")
	}
	// Замок старше минуты — остался от упавшего процесса: его убирают,
	// а следующая запись ротирует журнал.
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	_ = appendLog(root, entry(4), 100)
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("старый замок не удалён")
	}
	_ = appendLog(root, entry(5), 100)
	got, _ = ReadLog(root)
	if len(got) != 4 || got[len(got)-1].Rule != "f" {
		t.Fatalf("%+v", got)
	}

	// Большой порог: обычная запись не ротирует.
	if err := AppendLog(root, entry(6)); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadLog(root); len(got) != 5 {
		t.Fatalf("%d", len(got))
	}
}

func TestReadLogOldPartUnreadable(t *testing.T) {
	root := t.TempDir()
	// log.1.jsonl — папка: ошибка чтения, а не тихая потеря части журнала.
	if err := os.MkdirAll(OldLogPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLog(root); err == nil {
		t.Fatal("ожидалась ошибка")
	}
}

func TestPurgeRefs(t *testing.T) {
	dir := gitRepo(t, true)
	g, err := OpenGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := g.PurgeRefs(); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	now := time.Now()
	if _, err := g.CreateSnapshot(now, SnapshotMeta{}); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "changed")
	snap, err := g.CreateSnapshot(now.Add(time.Second), SnapshotMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.Rollback(snap, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "update-ref", "refs/heads/keep", "HEAD")
	refs, _ := g.AllRefs()
	if len(refs) < 2 {
		t.Fatalf("%v", refs)
	}
	n, err := g.PurgeRefs()
	if err != nil || n != len(refs) {
		t.Fatalf("%d %v", n, err)
	}
	if refs, _ := g.AllRefs(); len(refs) != 0 {
		t.Fatalf("остались %v", refs)
	}
	if out := git(t, dir, "show-ref", "refs/heads/keep"); out == "" {
		t.Fatal("удалена чужая ветка")
	}
}
