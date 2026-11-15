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
	Context  []string  `json:"context,omitempty"` // источники подсказок, показанных агенту
}

// MaxLogSize — размер журнала, после которого он ротируется: текущий файл
// переименовывается в log.1.jsonl (прежний log.1.jsonl при этом теряется),
// а новые записи идут в пустой log.jsonl. Так журнал занимает не больше
// двух таких размеров.
const MaxLogSize = 5 << 20 // 5 МБ

// LogPath — путь к журналу в репозитории root.
func LogPath(root string) string {
	return filepath.Join(root, StateDir, "log.jsonl")
}

// OldLogPath — путь к предыдущей части журнала после ротации.
func OldLogPath(root string) string {
	return filepath.Join(root, StateDir, "log.1.jsonl")
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
	return appendLog(root, e, MaxLogSize)
}

func appendLog(root string, e LogEntry, maxSize int64) error {
	if err := EnsureStateDir(root); err != nil {
		return err
	}
	rotateLog(root, maxSize)
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

// rotateLog переносит журнал в log.1.jsonl, если он не меньше maxSize.
//
// Хуки могут работать одновременно, поэтому ротацию выполняет только тот,
// кто создал файл-замок log.lock (O_EXCL — «создать, только если файла
// ещё нет»; это атомарно). Под замком размер проверяется заново: иначе
// второй процесс, увидевший большой файл до ротации, переименовал бы уже
// новый маленький журнал поверх старой части. Ошибки не мешают записи:
// в худшем случае журнал временно станет больше maxSize.
func rotateLog(root string, maxSize int64) {
	path := LogPath(root)
	if fi, err := os.Stat(path); err != nil || fi.Size() < maxSize {
		return
	}
	lock := filepath.Join(root, StateDir, "log.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		// Замок старше минуты остался от упавшего процесса: убираем его,
		// ротация случится при следующей записи.
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > time.Minute {
			_ = os.Remove(lock)
		}
		return
	}
	_ = f.Close()
	defer os.Remove(lock)
	if fi, err := os.Stat(path); err != nil || fi.Size() < maxSize {
		return // пока мы ждали, журнал уже ротировал другой процесс
	}
	// Процесс, который уже открыл старый файл, допишет строку в него, то есть
	// в log.1.jsonl: дескриптор указывает на файл, а не на имя. Запись не потеряется.
	_ = os.Rename(path, OldLogPath(root))
}

// ReadLog читает весь журнал: сначала log.1.jsonl, затем log.jsonl.
// Повреждённые строки пропускаются.
func ReadLog(root string) ([]LogEntry, error) {
	old, err := readLogFile(OldLogPath(root))
	if err != nil {
		return nil, err
	}
	cur, err := readLogFile(LogPath(root))
	return append(old, cur...), err
}

func readLogFile(path string) ([]LogEntry, error) {
	f, err := os.Open(path)
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
