package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/roma2246/holdfast/internal/config"
)

// HookInput is the JSON that Claude Code passes to a PreToolUse hook on stdin.
// Format: https://code.claude.com/docs/en/hooks
type HookInput struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"` // parsed later, depending on ToolName
}

// toolInput is the tool_input fields we care about across the different tools.
type toolInput struct {
	Command      string `json:"command"`       // Bash
	FilePath     string `json:"file_path"`     // Read, Edit, Write, MultiEdit
	NotebookPath string `json:"notebook_path"` // NotebookEdit
}

// hookOutput is the hook's reply. permissionDecision is "deny" | "ask" or empty
// (then Claude Code's regular permission check applies).
// additionalContext is text that Claude receives together with the result.
// omitempty leaves a field out of the JSON when it is empty.
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision,omitempty"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
		AdditionalContext        string `json:"additionalContext,omitempty"`
	} `json:"hookSpecificOutput"`
}

// HookEnv is the hook's external dependencies; tests replace them.
type HookEnv struct {
	Getenv func(string) string
	Getwd  func() (string, error)
	Now    func() time.Time
}

// DefaultHookEnv is the real process environment.
func DefaultHookEnv() HookEnv {
	return HookEnv{Getenv: os.Getenv, Getwd: os.Getwd, Now: time.Now}
}

// RunHook implements `holdfast guard check`. It reads JSON from stdin, applies
// the policy, writes the log and the reply. It always returns 0: the hook
// must not crash and break the agent's session.
//
// On "allow" the hook makes no decision: that way Claude Code carries on with
// its regular permission check instead of silently approving the action. If the
// touched files have project rules, the hook passes them to the agent as additionalContext.
func RunHook(stdin io.Reader, stdout io.Writer, env HookEnv) (code int) {
	var (
		root  string
		input HookInput
		entry LogEntry
	)
	// recover() catches a panic (the analogue of an unhandled exception).
	// It only works inside a deferred function.
	defer func() {
		if r := recover(); r != nil {
			v := Verdict{Ask, "internal-error", fmt.Sprintf("internal holdfast error (%v); the user needs to review this action", r)}
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
		v := Verdict{Ask, "bad-input", "holdfast could not read the hook request; the user needs to review this action"}
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

// evaluate is a function variable so that a test can replace it and check
// recovery after a panic.
var evaluate = Evaluate

// Evaluate applies the policy to one tool call. It also returns the policy
// itself (nil for tools that are not checked): the hook uses it to learn
// which files were touched.
func Evaluate(root string, input HookInput) (Verdict, *Policy) {
	var ti toolInput
	if len(input.ToolInput) > 0 {
		if err := json.Unmarshal(input.ToolInput, &ti); err != nil {
			return Verdict{Ask, "bad-input", "could not parse tool_input; the user needs to review this action"}, nil
		}
	}
	// The config is read lazily: only for the tools we check, and only the
	// [guard] section.
	var cfgErr error
	load := func() *Policy {
		cfg, err := config.LoadGuard(root)
		if err != nil {
			cfg, cfgErr = nil, err // the built-in rules keep working
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
	// The config exists but can't be read: the user's rules (protected paths,
	// command restrictions) are not in effect right now. Quietly running on the
	// built-in rules alone is not acceptable: a configuration mistake must not
	// weaken the protection. So everything that changes state needs
	// confirmation until the config is fixed.
	if cfgErr != nil && input.ToolName != "Read" {
		v = worst(v, Verdict{Ask, "config-error", fmt.Sprintf(
			"%s cannot be read (%v): the project rules are not applied. Fix the config; until then the agent's actions need confirmation",
			config.FileName, cfgErr)})
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
		_ = AppendLog(root, e) // a log error must not affect the decision
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
