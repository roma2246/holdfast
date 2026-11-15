package lint

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Severity — уровень находки.
type Severity int

// iota — счётчик внутри const-блока: Info=0, Warning=1, Error=2.
// Так получаются «перечисления» в Go.
const (
	Info Severity = iota
	Warning
	Error
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// MarshalJSON печатает уровень строкой, а не числом.
// Go вызывает этот метод автоматически, потому что тип реализует
// интерфейс json.Marshaler — интерфейсы в Go реализуются неявно.
func (s Severity) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// Finding — одна найденная проблема.
type Finding struct {
	RuleID   string   `json:"rule"`
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
}

// Rule — интерфейс правила lint. Чтобы добавить правило, создайте тип с
// этими двумя методами и вызовите Register в init() своего файла.
type Rule interface {
	ID() string
	Check(doc *Doc, repo *Repo) []Finding
}

// Describer — необязательный интерфейс: краткое описание правила для --list.
type Describer interface {
	Description() string
}

var (
	registryMu sync.Mutex
	registry   = map[string]Rule{}
)

// Register добавляет правило в общий реестр. Повторный ID — ошибка
// программиста, поэтому panic (срабатывает сразу при старте).
func Register(r Rule) {
	registryMu.Lock()
	defer registryMu.Unlock() // defer выполнится при выходе из функции
	id := r.ID()
	if _, dup := registry[id]; dup {
		panic(fmt.Sprintf("lint: rule %s registered twice", id))
	}
	registry[id] = r
}

// Rules возвращает все зарегистрированные правила, отсортированные по ID.
func Rules() []Rule {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Rule, 0, len(registry))
	for _, r := range registry {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// F — короткий конструктор Finding для правил.
func F(rule string, sev Severity, doc *Doc, line int, msg, hint string) Finding {
	return Finding{RuleID: rule, Severity: sev, File: doc.Path, Line: line, Message: msg, Hint: hint}
}

// SortFindings сортирует по файлу, строке и ID правила.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return strings.Compare(a.RuleID, b.RuleID) < 0
	})
}
