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

// PromptInput — поля JSON, который Claude Code передаёт хуку UserPromptSubmit.
type PromptInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt"`
}

// RunCheckpoint — реализация `holdfast guard checkpoint`: точка восстановления
// перед каждым запросом пользователя к агенту.
//
// Хук UserPromptSubmit не должен мешать работе: при успехе он ничего не
// печатает (обычный вывод этого хука Claude Code добавил бы в контекст
// агента), никогда не падает и молча пропускает папки без git.
//
// Если точку создать не удалось, молчать нельзя: пользователь считал бы, что
// страховка есть. Хук пишет ошибку в журнал, в .holdfast/checkpoint-error
// (её покажет `holdfast guard list`) и возвращает systemMessage — сообщение,
// которое Claude Code показывает пользователю.
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
		_ = json.Unmarshal(data, &in) // нет JSON — снимок всё равно полезен
	}
	root = projectRoot(HookInput{Cwd: in.Cwd}, env)
	g, err := OpenGit(root)
	if err != nil {
		root = ""
		return // не git-репозиторий: снимки невозможны, это не ошибка
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

// LastCheckpointError — текст ошибки последней неудачной точки ("" — всё в порядке).
func LastCheckpointError(root string) string {
	data, err := os.ReadFile(checkpointErrorPath(root))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
