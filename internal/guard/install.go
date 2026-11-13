package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// HookMatcher — инструменты, перед которыми вызывается хук.
const HookMatcher = "Bash|Edit|Write|MultiEdit|NotebookEdit|Read"

// DefaultHookCommand — команда, которую Claude Code запускает как хук.
const DefaultHookCommand = "holdfast guard check"

// InstallHook прописывает PreToolUse-хук в .claude/settings.json
// (или settings.local.json), не затирая другие настройки.
// Возвращает путь к файлу и changed=false, если хук уже был установлен.
func InstallHook(root string, local bool, command string) (path string, changed bool, err error) {
	name := "settings.json"
	if local {
		name = "settings.local.json"
	}
	path = filepath.Join(root, ".claude", name)

	// map[string]any — «произвольный JSON-объект»: так мы сохраним все
	// чужие ключи, о которых ничего не знаем.
	settings := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &settings); err != nil {
				return path, false, fmt.Errorf("%s: некорректный JSON: %w", path, err)
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return path, false, err
	}

	hooks, _ := settings["hooks"].(map[string]any) // «type assertion» с проверкой
	if hooks == nil {
		hooks = map[string]any{}
	}
	pre, _ := hooks["PreToolUse"].([]any)
	if hasHookCommand(pre, command) {
		return path, false, nil
	}
	pre = append(pre, map[string]any{
		"matcher": HookMatcher,
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": command,
			"timeout": 10,
		}},
	})
	hooks["PreToolUse"] = pre
	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return path, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, false, err
	}
	return path, true, os.WriteFile(path, append(out, '\n'), 0o644)
}

func hasHookCommand(pre []any, command string) bool {
	for _, group := range pre {
		g, _ := group.(map[string]any)
		hs, _ := g["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); c == command {
				return true
			}
		}
	}
	return false
}
