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

// hookOutput — ответ хука. permissionDecision: "allow" | "deny" | "ask".
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
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
// При «allow» хук ничего не печатает: так Claude Code продолжает обычную
// проверку разрешений, а не одобряет действие молча.
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
			writeDecision(stdout, v)
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
		finish(stdout, root, entry, v)
		return 0
	}

	v := evaluate(root, input)
	entry.Target = describeTarget(input)
	finish(stdout, root, entry, v)
	return 0
}

// evaluate — переменная-функция, чтобы тест мог подменить её и проверить
// восстановление после panic.
var evaluate = Evaluate

// Evaluate применяет политику к одному вызову инструмента.
func Evaluate(root string, input HookInput) Verdict {
	var ti toolInput
	if len(input.ToolInput) > 0 {
		if err := json.Unmarshal(input.ToolInput, &ti); err != nil {
			return Verdict{Ask, "bad-input", "не удалось разобрать tool_input — нужна проверка пользователем"}
		}
	}
	// Конфиг читаем лениво: только для инструментов, которые проверяем,
	// и только секцию [guard].
	load := func() *Policy {
		cfg, err := config.LoadGuard(root)
		if err != nil {
			cfg = nil // битый конфиг — работаем со встроенной политикой
		}
		return NewPolicy(root, input.Cwd, cfg)
	}
	switch input.ToolName {
	case "Bash":
		return load().CheckCommand(ti.Command)
	case "Edit", "Write", "MultiEdit":
		return load().CheckFile(OpWrite, ti.FilePath)
	case "NotebookEdit":
		return load().CheckFile(OpWrite, ti.NotebookPath)
	case "Read":
		return load().CheckFile(OpRead, ti.FilePath)
	}
	return allowVerdict
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

func finish(w io.Writer, root string, e LogEntry, v Verdict) {
	e.Decision, e.Rule, e.Reason = v.Decision.String(), v.Rule, v.Reason
	if root != "" {
		_ = AppendLog(root, e) // ошибка журнала не должна влиять на решение
	}
	if v.Decision != Allow {
		writeDecision(w, v)
	}
}

func writeDecision(w io.Writer, v Verdict) {
	var out hookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = v.Decision.String()
	out.HookSpecificOutput.PermissionDecisionReason = "holdfast [" + v.Rule + "]: " + v.Reason
	_ = json.NewEncoder(w).Encode(out)
}
