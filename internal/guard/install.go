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

	"mvdan.cc/sh/v3/syntax"
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
	path = SettingsPath(root, local)
	settings, err := readSettings(path)
	if err != nil {
		return path, false, err
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

	return path, true, writeSettings(path, settings)
}

// SettingsPath — .claude/settings.json или .claude/settings.local.json.
func SettingsPath(root string, local bool) string {
	name := "settings.json"
	if local {
		name = "settings.local.json"
	}
	return filepath.Join(root, ".claude", name)
}

// readSettings читает настройки Claude Code; нет файла — пустые настройки.
func readSettings(path string) (map[string]any, error) {
	// map[string]any — «произвольный JSON-объект»: так мы сохраним все
	// чужие ключи, о которых ничего не знаем.
	settings := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &settings); err != nil {
				return nil, fmt.Errorf("%s: некорректный JSON: %w", path, err)
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if settings == nil {
		settings = map[string]any{} // в файле было JSON-значение null
	}
	return settings, nil
}

func writeSettings(path string, settings map[string]any) error {
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// InstalledHook — хук holdfast, найденный в настройках Claude Code.
type InstalledHook struct {
	File    string // путь к settings.json или settings.local.json
	Event   string // PreToolUse или UserPromptSubmit
	Command string // как записано в настройках
	Bin     string // программа, которую запустит Claude Code (кавычки сняты)
}

// parseHoldfastHook распознаёт команду хука holdfast: `<bin> guard check`
// или `<bin> guard checkpoint`, где имя файла bin начинается с «holdfast»
// (так установка с --bin /полный/путь/holdfast тоже узнаётся). Команда
// разбирается через AST shell, чтобы правильно снять кавычки с пути.
func parseHoldfastHook(command string) (bin, sub string, ok bool) {
	file, err := parseShell(command)
	if err != nil || len(file.Stmts) != 1 {
		return "", "", false
	}
	call, isCall := file.Stmts[0].Cmd.(*syntax.CallExpr)
	if !isCall || len(call.Assigns) > 0 || len(file.Stmts[0].Redirs) > 0 {
		return "", "", false
	}
	w := callWords(call)
	if len(w) != 3 || w[0].dynamic || w[1].s != "guard" || (w[2].s != "check" && w[2].s != "checkpoint") {
		return "", "", false
	}
	if !strings.HasPrefix(filepath.Base(w[0].s), "holdfast") {
		return "", "", false
	}
	return w[0].s, w[2].s, true
}

// hookEvents — какое событие Claude Code какой подкомандой обслуживается.
var hookEvents = map[string]string{"PreToolUse": "check", "UserPromptSubmit": "checkpoint"}

// FindHooks ищет хуки holdfast в .claude/settings.json и settings.local.json.
// Хук считается подключённым, только если подкоманда соответствует событию:
// `guard checkpoint` в PreToolUse не создаст ни одной точки.
func FindHooks(root string) ([]InstalledHook, error) {
	var (
		found []InstalledHook
		errs  []error
	)
	for _, local := range []bool{false, true} {
		path := SettingsPath(root, local)
		settings, err := readSettings(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		hooks, _ := settings["hooks"].(map[string]any)
		for event, sub := range hookEvents {
			list, _ := hooks[event].([]any)
			for _, group := range list {
				g, _ := group.(map[string]any)
				hs, _ := g["hooks"].([]any)
				for _, h := range hs {
					hm, _ := h.(map[string]any)
					c, _ := hm["command"].(string)
					if bin, s, ok := parseHoldfastHook(c); ok && s == sub {
						found = append(found, InstalledHook{File: path, Event: event, Command: c, Bin: bin})
					}
				}
			}
		}
	}
	// errors.Join объединяет несколько ошибок в одну (nil, если их нет).
	return found, errors.Join(errs...)
}

// UninstallHook убирает из .claude/settings.json (или settings.local.json)
// хуки holdfast — обратное к InstallHook. Остальные настройки и чужие хуки
// не трогаются; опустевшие группы, события и ключ "hooks" удаляются, чтобы
// после install + uninstall файл выглядел как до установки.
// removed — сколько команд удалено (0 — хуков holdfast в файле не было).
func UninstallHook(root string, local bool) (path string, removed int, err error) {
	path = SettingsPath(root, local)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return path, 0, nil
	}
	settings, err := readSettings(path)
	if err != nil {
		return path, 0, err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	for event, list := range hooks {
		groups, ok := list.([]any)
		if !ok {
			continue
		}
		var keptGroups []any
		for _, group := range groups {
			g, ok := group.(map[string]any)
			hs, isList := g["hooks"].([]any)
			if !ok || !isList {
				keptGroups = append(keptGroups, group) // чужой формат — не трогаем
				continue
			}
			var kept []any
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				c, _ := hm["command"].(string)
				if _, _, ours := parseHoldfastHook(c); ours {
					removed++
					continue
				}
				kept = append(kept, h)
			}
			if len(kept) == len(hs) {
				keptGroups = append(keptGroups, group)
			} else if len(kept) > 0 {
				g["hooks"] = kept
				keptGroups = append(keptGroups, g)
			}
		}
		if len(keptGroups) == 0 {
			delete(hooks, event) // удалять ключи map внутри range в Go безопасно
		} else {
			hooks[event] = keptGroups
		}
	}
	if removed == 0 {
		return path, 0, nil
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	return path, removed, writeSettings(path, settings)
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
