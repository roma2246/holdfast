package guard

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StateDir — папка Holdfast внутри репозитория (журнал и служебные файлы).
const StateDir = ".holdfast"

// LogEntry — одна строка журнала .holdfast/log.jsonl.
type LogEntry struct {
	Time     time.Time `json:"time"`
	Session  string    `json:"session,omitempty"`
	Tool     string    `json:"tool"`
	Target   string    `json:"target"` // команда или путь к файлу
	Decision string    `json:"decision"`
	Rule     string    `json:"rule,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// LogPath — путь к журналу в репозитории root.
func LogPath(root string) string {
	return filepath.Join(root, StateDir, "log.jsonl")
}

// EnsureStateDir создаёт .holdfast/ с .gitignore внутри, чтобы журнал
// не попадал ни в коммиты, ни в снапшоты.
func EnsureStateDir(root string) error {
	dir := filepath.Join(root, StateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); errors.Is(err, fs.ErrNotExist) {
		return os.WriteFile(gi, []byte("*\n"), 0o644)
	}
	return nil
}

// AppendLog дописывает запись в журнал. JSONL — «JSON Lines»: один
// JSON-объект на строку, файл легко дописывать и читать построчно.
func AppendLog(root string, e LogEntry) error {
	if err := EnsureStateDir(root); err != nil {
		return err
	}
	// O_APPEND: запись атомарно добавляется в конец, даже если несколько
	// хуков пишут одновременно (для строк меньше PIPE_BUF).
	f, err := os.OpenFile(LogPath(root), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadLog читает весь журнал. Повреждённые строки пропускаются.
func ReadLog(root string) ([]LogEntry, error) {
	f, err := os.Open(LogPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []LogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // длинные команды
	for sc.Scan() {
		var e LogEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
