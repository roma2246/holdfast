package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HookMatcher — инструменты, перед которыми вызывается хук.
const HookMatcher = "Bash|Edit|Write|MultiEdit|NotebookEdit|Read"

// DefaultBin — как Claude Code вызывает holdfast (бинарник из PATH).
const DefaultBin = "holdfast"

// InstallHook прописывает в .claude/settings.json (или settings.local.json)
// два хука, не затирая другие настройки:
//   - PreToolUse: `<bin> guard check` — проверка каждого действия агента;
//   - UserPromptSubmit: `<bin> guard checkpoint` — точка восстановления
//     перед каждым запросом пользователя.
//
// Повторная установка ничего не дублирует; changed=false — всё уже было.
func InstallHook(root string, local bool, bin string) (path string, changed bool, err error) {
	if bin == "" {
		bin = DefaultBin
	}
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
	if settings == nil {
		settings = map[string]any{} // в файле было JSON-значение null
	}

	hooks, _ := settings["hooks"].(map[string]any) // «type assertion» с проверкой
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, h := range []struct {
		event, matcher, command string
		timeout                 int
	}{
		{"PreToolUse", HookMatcher, shellQuoteArg(bin) + " guard check", 10},
		// Снимок большого репозитория может занять секунды — запас по времени.
		{"UserPromptSubmit", "", shellQuoteArg(bin) + " guard checkpoint", 30},
	} {
		list, _ := hooks[h.event].([]any)
		if hasHookCommand(list, h.command) {
			continue
		}
		group := map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": h.command, "timeout": h.timeout,
		}}}
		if h.matcher != "" {
			group["matcher"] = h.matcher
		}
		hooks[h.event] = append(list, group)
		changed = true
	}
	if !changed {
		return path, false, nil
	}
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

var shellSafeRe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuoteArg экранирует путь для команды хука: Claude Code запускает её
// через shell, и путь с пробелом без кавычек распался бы на два слова.
func shellQuoteArg(s string) string {
	if shellSafeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
