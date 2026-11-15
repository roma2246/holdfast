package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/roma2246/holdfast/internal/config"
)

// PromptInput is the fields of the JSON that Claude Code passes to a UserPromptSubmit hook.
type PromptInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt"`
}

// RunCheckpoint implements `holdfast guard checkpoint`: a restore point
// before every prompt the user sends to the agent.
//
// The UserPromptSubmit hook must stay out of the way: on success it prints
// nothing (Claude Code would add this hook's regular output to the agent's
// context), it never crashes and it silently skips directories without git.
//
// If the restore point could not be created, staying silent is not an option:
// the user would believe the safety net is there. The hook writes the error to
// the log and to .holdfast/checkpoint-error (`holdfast guard list` shows it)
// and returns a systemMessage, a message that Claude Code shows to the user.
func RunCheckpoint(stdin io.Reader, stdout io.Writer, env HookEnv) {
	var (
		in   PromptInput
		root string
	)
	fail := func(reason string) {
		if root == "" {
			return
		}
		_ = AppendLog(root, LogEntry{Time: env.Now().UTC(), Session: in.SessionID, Tool: "UserPromptSubmit",
			Target: oneLine(in.Prompt, 120), Decision: "snapshot", Rule: "error", Reason: reason})
		_ = os.WriteFile(checkpointErrorPath(root), []byte(env.Now().Format(time.RFC3339)+" "+reason+"\n"), 0o644)
		_ = json.NewEncoder(stdout).Encode(map[string]string{
			"systemMessage": "holdfast: NO restore point was created before this prompt (" + oneLine(reason, 200) +
				"). The changes made for this prompt can't be rolled back until the error is fixed.",
		})
	}
	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Sprint("internal error: ", r))
		}
	}()

	data, err := io.ReadAll(io.LimitReader(stdin, 16<<20))
	if err == nil {
		_ = json.Unmarshal(data, &in) // no JSON: a snapshot is still useful
	}
	root = projectRoot(HookInput{Cwd: in.Cwd}, env)
	g, err := OpenGit(root)
	if err != nil {
		root = ""
		return // not a git repository: snapshots are impossible, and that is not an error
	}
	if err := EnsureStateDir(root); err != nil {
		fail(err.Error())
		return
	}
	keep := config.DefaultKeepSnapshots
	if cfg, err := config.LoadGuard(root); err == nil {
		keep = cfg.KeepSnapshots
	}

	now := env.Now()
	snap, created, err := g.Checkpoint(now, SnapshotMeta{Kind: KindCheckpoint, Session: in.SessionID, Prompt: in.Prompt}, keep)
	if err != nil {
		fail(err.Error())
		return
	}
	_ = os.Remove(checkpointErrorPath(root))
	entry := LogEntry{Time: now.UTC(), Session: in.SessionID, Tool: "UserPromptSubmit", Target: oneLine(in.Prompt, 120),
		Decision: "snapshot", Rule: "unchanged", Reason: "unchanged since " + snap.Name()}
	if created {
		entry.Rule, entry.Reason = "checkpoint", snap.Name()
	}
	_ = AppendLog(root, entry)
}

func checkpointErrorPath(root string) string {
	return filepath.Join(root, StateDir, "checkpoint-error")
}

// LastCheckpointError is the error text of the last failed restore point ("" means all is well).
func LastCheckpointError(root string) string {
	data, err := os.ReadFile(checkpointErrorPath(root))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
