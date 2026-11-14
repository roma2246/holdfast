package guard

import (
	"encoding/json"
	"fmt"
	"io"

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
// Хук UserPromptSubmit не должен мешать работе: он ничего не печатает
// (вывод этого хука Claude Code добавил бы в контекст агента), никогда не
// падает и молча пропускает папки без git. Ошибки пишутся в журнал.
func RunCheckpoint(stdin io.Reader, env HookEnv) {
	var (
		in   PromptInput
		root string
	)
	defer func() {
		if r := recover(); r != nil && root != "" {
			_ = AppendLog(root, LogEntry{Time: env.Now().UTC(), Session: in.SessionID, Tool: "UserPromptSubmit",
				Decision: "snapshot", Rule: "internal-error", Reason: fmt.Sprint(r)})
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
	if err := EnsureStateDir(g.Dir); err != nil {
		return
	}
	keep := config.DefaultKeepSnapshots
	if cfg, err := config.LoadGuard(root); err == nil {
		keep = cfg.KeepSnapshots
	}

	now := env.Now()
	entry := LogEntry{Time: now.UTC(), Session: in.SessionID, Tool: "UserPromptSubmit", Target: oneLine(in.Prompt, 120), Decision: "snapshot"}
	snap, created, err := g.Checkpoint(now, SnapshotMeta{Kind: KindCheckpoint, Session: in.SessionID, Prompt: in.Prompt}, keep)
	switch {
	case err != nil:
		entry.Rule, entry.Reason = "error", err.Error()
	default:
		entry.Rule, entry.Reason = "unchanged", "без изменений с "+snap.Name()
		if created {
			entry.Rule, entry.Reason = "checkpoint", snap.Name()
		}
		if err := MarkSnapshot(root, in.SessionID, snap, now); err != nil {
			entry.Rule, entry.Reason = "error", err.Error()
		}
	}
	_ = AppendLog(root, entry)
}
