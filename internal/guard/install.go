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

// HookMatcher is the tools the hook is called before.
const HookMatcher = "Bash|Edit|Write|MultiEdit|NotebookEdit|Read"

// DefaultBin is how Claude Code invokes holdfast (the binary from PATH).
const DefaultBin = "holdfast"

// InstallHook registers two hooks in .claude/settings.json (or
// settings.local.json) without clobbering other settings:
//   - PreToolUse: `<bin> guard check` checks every action of the agent;
//   - UserPromptSubmit: `<bin> guard checkpoint` takes a restore point
//     before every user prompt.
//
// Installing again duplicates nothing; changed=false means everything was already there.
func InstallHook(root string, local bool, bin string) (path string, changed bool, err error) {
	if bin == "" {
		bin = DefaultBin
	}
	path = SettingsPath(root, local)
	settings, err := readSettings(path)
	if err != nil {
		return path, false, err
	}

	hooks, _ := settings["hooks"].(map[string]any) // a "type assertion" with a check
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, h := range []struct {
		event, matcher, command string
		timeout                 int
	}{
		{"PreToolUse", HookMatcher, shellQuoteArg(bin) + " guard check", 10},
		// A snapshot of a large repository can take seconds, hence the generous timeout.
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

// SettingsPath is .claude/settings.json or .claude/settings.local.json.
func SettingsPath(root string, local bool) string {
	name := "settings.json"
	if local {
		name = "settings.local.json"
	}
	return filepath.Join(root, ".claude", name)
}

// readSettings reads the Claude Code settings; a missing file means empty settings.
func readSettings(path string) (map[string]any, error) {
	// map[string]any is "an arbitrary JSON object": this way we keep all the
	// foreign keys we know nothing about.
	settings := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &settings); err != nil {
				return nil, fmt.Errorf("%s: invalid JSON: %w", path, err)
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if settings == nil {
		settings = map[string]any{} // the file held the JSON value null
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

// InstalledHook is a holdfast hook found in the Claude Code settings.
type InstalledHook struct {
	File    string // path to settings.json or settings.local.json
	Event   string // PreToolUse or UserPromptSubmit
	Command string // as written in the settings
	Bin     string // the program Claude Code will run (quotes removed)
}

// parseHoldfastHook recognizes a holdfast hook command: `<bin> guard check`
// or `<bin> guard checkpoint`, where the file name of bin starts with
// "holdfast" (so an install with --bin /full/path/holdfast is recognized too).
// The command is parsed through the shell AST to unquote the path correctly.
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

// hookEvents maps each Claude Code event to the subcommand that serves it.
var hookEvents = map[string]string{"PreToolUse": "check", "UserPromptSubmit": "checkpoint"}

// FindHooks looks for holdfast hooks in .claude/settings.json and settings.local.json.
// A hook counts as connected only if the subcommand matches the event:
// `guard checkpoint` under PreToolUse would never create a restore point.
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
	// errors.Join combines several errors into one (nil if there are none).
	return found, errors.Join(errs...)
}

// UninstallHook removes the holdfast hooks from .claude/settings.json (or
// settings.local.json): the inverse of InstallHook. Other settings and foreign
// hooks are left alone; emptied groups, events and the "hooks" key are removed
// so that after install + uninstall the file looks the way it did before.
// removed is how many commands were removed (0 means the file had no holdfast hooks).
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
				keptGroups = append(keptGroups, group) // a foreign format: leave it alone
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
			delete(hooks, event) // deleting map keys inside a range loop is safe in Go
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

// shellQuoteArg quotes a path for the hook command: Claude Code runs it
// through a shell, and an unquoted path with a space would split into two words.
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
