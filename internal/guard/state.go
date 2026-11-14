package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Отметки о снимках текущей задачи: .holdfast/checkpoints/<session>.json
// (точка восстановления перед запросом) и manual.json (holdfast guard start).
//
// Хуку нужно быстро ответить «есть ли свежий снимок» — без запуска git.
// Просто «есть ли хоть какой-то снимок» не годится: вчерашний снимок не
// защитит файлы, появившиеся сегодня.

// ManualFreshness — сколько ручной снимок считается свежим. Хватает, чтобы
// агент по подсказке сделал `holdfast guard start` и повторил команду.
const ManualFreshness = 15 * time.Minute

type snapshotMark struct {
	Ref     string    `json:"ref"`
	Time    time.Time `json:"time"`
	Session string    `json:"session,omitempty"`
}

func markPath(root, session string) string {
	name := unsafeSessionRe.ReplaceAllString(session, "")
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		name = "manual"
	}
	return filepath.Join(root, StateDir, "checkpoints", name+".json")
}

// MarkSnapshot запоминает снимок как точку текущей задачи сессии
// (session == "" — ручной снимок).
func MarkSnapshot(root, session string, snap Snapshot, now time.Time) error {
	if err := EnsureStateDir(root); err != nil {
		return err
	}
	path := markPath(root, session)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(snapshotMark{Ref: snap.Ref, Time: now.UTC(), Session: session})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readMark(root, session string) (snapshotMark, bool) {
	var m snapshotMark
	data, err := os.ReadFile(markPath(root, session))
	if err != nil || json.Unmarshal(data, &m) != nil || m.Ref == "" {
		return m, false
	}
	// Снимок могли удалить (Prune, git gc) — тогда отметка ничего не стоит.
	return m, refExists(findGitRoot(root), m.Ref)
}

// freshSnapshot — есть ли снимок, сделанный для текущей задачи: точка перед
// последним запросом этой сессии или недавний ручной снимок.
func freshSnapshot(root, session string, now time.Time) bool {
	if session != "" {
		if _, ok := readMark(root, session); ok {
			return true
		}
	}
	m, ok := readMark(root, "")
	return ok && now.Sub(m.Time) < ManualFreshness
}
