package guard

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/roma2246/holdfast/internal/config"
)

// Подсказки «в момент действия»: когда агент читает или меняет файл,
// хук добавляет в его контекст правила, которые касаются именно этого
// файла — из [[guard.context]] в holdfast.toml и из строк CLAUDE.md /
// AGENTS.md, где упомянут этот путь. Каждая подсказка показывается
// один раз за сессию, чтобы не засорять контекст.

const (
	maxContextNotes = 5
	maxNoteLen      = 400
)

// Note — одна подсказка для агента.
type Note struct {
	Source string // "holdfast.toml" или "CLAUDE.md:12"
	Text   string
}

var (
	ctxCodeRe  = regexp.MustCompile("`([^`\\s]+)`")
	ctxPlainRe = regexp.MustCompile(`(?:^|[\s(«"'])((?:\./)?[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-/*]*)`)
	ctxFenceRe = regexp.MustCompile("^\\s*(```|~~~)")
)

// ContextNotes возвращает подсказки для файлов, затронутых последней проверкой.
func (p *Policy) ContextNotes() []Note {
	var rels []string
	for _, abs := range p.touched {
		if p.inRepo(abs) && abs != p.Root {
			rels = append(rels, p.rel(abs))
		}
	}
	if len(rels) == 0 {
		return nil
	}
	matches := func(patterns ...string) bool {
		for _, pat := range patterns {
			for _, r := range rels {
				if p.match(pat, r) {
					return true
				}
			}
		}
		return false
	}

	var notes []Note
	for _, c := range p.cfg.Context {
		if strings.TrimSpace(c.Note) != "" && matches(c.Paths...) {
			notes = append(notes, Note{config.FileName, strings.TrimSpace(c.Note)})
		}
	}
	for _, name := range config.DefaultLintFiles {
		for _, l := range instructionLines(filepath.Join(p.Root, name)) {
			if matches(l.paths...) {
				notes = append(notes, Note{fmt.Sprintf("%s:%d", name, l.line), l.text})
			}
		}
	}
	if len(notes) > maxContextNotes {
		notes = notes[:maxContextNotes]
	}
	return notes
}

type instrLine struct {
	line  int
	text  string
	paths []string
}

// instructionLines — строки файла инструкций, в которых упомянуты пути.
func instructionLines(path string) []instrLine {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []instrLine
	inFence, inComment := false, false
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		switch {
		case ctxFenceRe.MatchString(line):
			inFence = !inFence
			continue
		case strings.Contains(line, "<!--") && !strings.Contains(line, "-->"):
			inComment = true
			continue
		case inComment:
			inComment = !strings.Contains(line, "-->")
			continue
		case inFence || strings.Contains(line, "<!--") || strings.HasPrefix(strings.TrimSpace(line), "#"):
			continue
		}
		var paths []string
		for _, m := range ctxCodeRe.FindAllStringSubmatch(line, -1) {
			if strings.ContainsAny(m[1], "/*") && !strings.Contains(m[1], "://") {
				paths = append(paths, normPattern(m[1]))
			}
		}
		for _, m := range ctxPlainRe.FindAllStringSubmatch(ctxCodeRe.ReplaceAllString(line, " "), -1) {
			if !strings.Contains(m[1], "://") {
				paths = append(paths, normPattern(m[1]))
			}
		}
		if len(paths) > 0 {
			text := strings.TrimSpace(line)
			if r := []rune(text); len(r) > maxNoteLen {
				text = string(r[:maxNoteLen-1]) + "…"
			}
			out = append(out, instrLine{n, text, paths})
		}
	}
	return out
}

func normPattern(s string) string {
	s = strings.TrimRight(strings.TrimPrefix(s, "./"), ".,;:)»\"'")
	if strings.HasSuffix(s, "/") {
		s += "**"
	}
	return s
}

// FormatNotes превращает подсказки в текст для additionalContext.
func FormatNotes(notes []Note) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("holdfast: project rules for the files you are working with right now:\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "- [%s] %s\n", n.Source, n.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

var unsafeSessionRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// FilterSeen убирает подсказки, уже показанные в этой сессии, и запоминает
// новые в .holdfast/seen/<session>. Без session_id ничего не фильтрует.
func FilterSeen(root, session string, notes []Note) []Note {
	session = unsafeSessionRe.ReplaceAllString(session, "")
	if session == "" || len(notes) == 0 {
		return notes
	}
	if len(session) > 64 {
		session = session[:64]
	}
	dir := filepath.Join(root, StateDir, "seen")
	path := filepath.Join(dir, session)
	seen := map[string]bool{}
	if data, err := os.ReadFile(path); err == nil {
		for _, k := range strings.Split(string(data), "\n") {
			seen[k] = true
		}
	}
	var fresh []Note
	var keys []string
	for _, n := range notes {
		h := fnv.New64a() // быстрый некриптографический хэш
		h.Write([]byte(n.Source + "\x00" + n.Text))
		k := fmt.Sprintf("%x", h.Sum64())
		if !seen[k] {
			seen[k] = true
			fresh = append(fresh, n)
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 && EnsureStateDir(root) == nil && os.MkdirAll(dir, 0o755) == nil {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(strings.Join(keys, "\n") + "\n")
			f.Close()
		}
	}
	return fresh
}
