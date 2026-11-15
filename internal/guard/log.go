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

// StateDir is Holdfast's directory inside the repository (the log and service files).
const StateDir = ".holdfast"

// LogEntry is one line of the log .holdfast/log.jsonl.
type LogEntry struct {
	Time     time.Time `json:"time"`
	Session  string    `json:"session,omitempty"`
	Tool     string    `json:"tool"`
	Target   string    `json:"target"` // a command or a file path
	Decision string    `json:"decision"`
	Rule     string    `json:"rule,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Context  []string  `json:"context,omitempty"` // sources of the hints shown to the agent
}

// MaxLogSize is the log size at which it is rotated: the current file is
// renamed to log.1.jsonl (the previous log.1.jsonl is lost in the process),
// and new entries go to an empty log.jsonl. This way the log takes up at most
// twice this size.
const MaxLogSize = 5 << 20 // 5 MB

// LogPath is the path to the log in the repository root.
func LogPath(root string) string {
	return filepath.Join(root, StateDir, "log.jsonl")
}

// OldLogPath is the path to the previous part of the log after rotation.
func OldLogPath(root string) string {
	return filepath.Join(root, StateDir, "log.1.jsonl")
}

// EnsureStateDir creates .holdfast/ with a .gitignore inside, so that the log
// ends up neither in commits nor in snapshots.
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

// AppendLog appends an entry to the log. JSONL is "JSON Lines": one JSON
// object per line, a file that is easy to append to and to read line by line.
func AppendLog(root string, e LogEntry) error {
	return appendLog(root, e, MaxLogSize)
}

func appendLog(root string, e LogEntry, maxSize int64) error {
	if err := EnsureStateDir(root); err != nil {
		return err
	}
	rotateLog(root, maxSize)
	// O_APPEND: the entry is atomically added at the end, even when several
	// hooks write at the same time (for lines shorter than PIPE_BUF).
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

// rotateLog moves the log to log.1.jsonl once it is at least maxSize.
//
// Hooks may run concurrently, so the rotation is done only by whoever created
// the lock file log.lock (O_EXCL means "create only if the file does not exist
// yet"; that is atomic). Under the lock the size is checked again: otherwise a
// second process that saw the big file before the rotation would rename the
// new small log over the old part. Errors don't get in the way of writing:
// in the worst case the log temporarily grows beyond maxSize.
func rotateLog(root string, maxSize int64) {
	path := LogPath(root)
	if fi, err := os.Stat(path); err != nil || fi.Size() < maxSize {
		return
	}
	lock := filepath.Join(root, StateDir, "log.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		// A lock older than a minute was left by a crashed process: remove it,
		// the rotation will happen on the next write.
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > time.Minute {
			_ = os.Remove(lock)
		}
		return
	}
	_ = f.Close()
	defer os.Remove(lock)
	if fi, err := os.Stat(path); err != nil || fi.Size() < maxSize {
		return // while we were waiting, another process already rotated the log
	}
	// A process that already has the old file open will append its line there,
	// that is to log.1.jsonl: a descriptor points at the file, not the name. The entry is not lost.
	_ = os.Rename(path, OldLogPath(root))
}

// ReadLog reads the whole log: first log.1.jsonl, then log.jsonl.
// Corrupted lines are skipped.
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
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // long commands
	for sc.Scan() {
		var e LogEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
