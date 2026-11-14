package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/roma2246/holdfast/internal/config"
)

// HookInput — JSON, который Claude Code передаёт PreToolUse-хуку в stdin.
// Формат: https://code.claude.com/docs/en/hooks
type HookInput struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"` // разбираем позже, по ToolName
}

// toolInput — поля tool_input, которые нас интересуют у разных инструментов.
type toolInput struct {
	Command      string `json:"command"`       // Bash
	FilePath     string `json:"file_path"`     // Read, Edit, Write, MultiEdit
	NotebookPath string `json:"notebook_path"` // NotebookEdit
}

// hookOutput — ответ хука. permissionDecision: "deny" | "ask" или пусто
// (тогда работает обычная проверка разрешений Claude Code).
// additionalContext — текст, который Claude получит вместе с результатом.
// omitempty — не выводить поле в JSON, если оно пустое.
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision,omitempty"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
		AdditionalContext        string `json:"additionalContext,omitempty"`
	} `json:"hookSpecificOutput"`
}

// HookEnv — внешние зависимости хука; в тестах подменяются.
type HookEnv struct {
	Getenv func(string) string
	Getwd  func() (string, error)
	Now    func() time.Time
}

// DefaultHookEnv — реальное окружение процесса.
func DefaultHookEnv() HookEnv {
	return HookEnv{Getenv: os.Getenv, Getwd: os.Getwd, Now: time.Now}
}

// RunHook — реализация `holdfast guard check`. Читает JSON из stdin,
// применяет политику, пишет журнал и ответ. Всегда возвращает 0: хук
// не должен «падать» и ломать сессию агента.
//
// При «allow» хук не выносит решения: так Claude Code продолжает обычную
// проверку разрешений, а не одобряет действие молча. Если для затронутых
// файлов есть правила проекта, хук передаёт их агенту как additionalContext.
func RunHook(stdin io.Reader, stdout io.Writer, env HookEnv) (code int) {
	var (
		root  string
		input HookInput
		entry LogEntry
	)
	// recover() перехватывает panic (аналог необработанного исключения).
	// Он работает только внутри отложенной (defer) функции.
	defer func() {
		if r := recover(); r != nil {
			v := Verdict{Ask, "internal-error", fmt.Sprintf("внутренняя ошибка holdfast (%v) — нужна проверка пользователем", r)}
			writeOutput(stdout, v, "")
			if root != "" {
				entry.Decision, entry.Rule, entry.Reason = v.Decision.String(), v.Rule, v.Reason
				_ = AppendLog(root, entry)
			}
			code = 0
		}
	}()

	data, err := io.ReadAll(io.LimitReader(stdin, 16<<20))
	if err == nil {
		err = json.Unmarshal(data, &input)
	}
	root = projectRoot(input, env)
	entry = LogEntry{Time: env.Now().UTC(), Session: input.SessionID, Tool: input.ToolName}
	if err != nil {
		v := Verdict{Ask, "bad-input", "holdfast не смог прочитать запрос хука — нужна проверка пользователем"}
		finish(stdout, root, entry, v, nil)
		return 0
	}

	v, pol := evaluate(root, input)
	entry.Target = describeTarget(input)
	var notes []Note
	if pol != nil && v.Decision != Deny {
		notes = FilterSeen(root, input.SessionID, pol.ContextNotes())
	}
	finish(stdout, root, entry, v, notes)
	return 0
}

// evaluate — переменная-функция, чтобы тест мог подменить её и проверить
// восстановление после panic.
var evaluate = Evaluate

// Evaluate применяет политику к одному вызову инструмента. Возвращает
// и саму политику (nil для непроверяемых инструментов) — по ней хук
// узнаёт, какие файлы затронуты.
func Evaluate(root string, input HookInput) (Verdict, *Policy) {
	var ti toolInput
	if len(input.ToolInput) > 0 {
		if err := json.Unmarshal(input.ToolInput, &ti); err != nil {
			return Verdict{Ask, "bad-input", "не удалось разобрать tool_input — нужна проверка пользователем"}, nil
		}
	}
	// Конфиг читаем лениво: только для инструментов, которые проверяем,
	// и только секцию [guard].
	load := func() *Policy {
		cfg, err := config.LoadGuard(root)
		if err != nil {
			cfg = nil // битый конфиг — работаем со встроенной политикой
		}
		p := NewPolicy(root, input.Cwd, cfg)
		p.Session = input.SessionID
		return p
	}
	var p *Policy
	var v Verdict
	switch input.ToolName {
	case "Bash":
		p = load()
		v = p.CheckCommand(ti.Command)
	case "Edit", "Write", "MultiEdit":
		p = load()
		v = p.CheckFile(OpWrite, ti.FilePath)
	case "NotebookEdit":
		p = load()
		v = p.CheckFile(OpWrite, ti.NotebookPath)
	case "Read":
		p = load()
		v = p.CheckFile(OpRead, ti.FilePath)
	default:
		return allowVerdict, nil
	}
	return v, p
}

func describeTarget(in HookInput) string {
	var ti toolInput
	_ = json.Unmarshal(in.ToolInput, &ti)
	switch {
	case ti.Command != "":
		return ti.Command
	case ti.FilePath != "":
		return ti.FilePath
	default:
		return ti.NotebookPath
	}
}

func projectRoot(in HookInput, env HookEnv) string {
	if d := env.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		return d
	}
	if in.Cwd != "" {
		return in.Cwd
	}
	wd, err := env.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func finish(w io.Writer, root string, e LogEntry, v Verdict, notes []Note) {
	e.Decision, e.Rule, e.Reason = v.Decision.String(), v.Rule, v.Reason
	for _, n := range notes {
		e.Context = append(e.Context, n.Source)
	}
	if root != "" {
		_ = AppendLog(root, e) // ошибка журнала не должна влиять на решение
	}
	ctx := FormatNotes(notes)
	if v.Decision != Allow || ctx != "" {
		writeOutput(w, v, ctx)
	}
}

func writeOutput(w io.Writer, v Verdict, ctx string) {
	var out hookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	if v.Decision != Allow {
		out.HookSpecificOutput.PermissionDecision = v.Decision.String()
		out.HookSpecificOutput.PermissionDecisionReason = "holdfast [" + v.Rule + "]: " + v.Reason
	}
	out.HookSpecificOutput.AdditionalContext = ctx
	_ = json.NewEncoder(w).Encode(out)
}
