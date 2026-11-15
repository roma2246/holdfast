package lint

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Severity is the level of a finding.
type Severity int

// iota is a counter inside a const block: Info=0, Warning=1, Error=2.
// This is how "enumerations" are made in Go.
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

// MarshalJSON prints the level as a string rather than a number.
// Go calls this method automatically because the type implements the
// json.Marshaler interface: interfaces in Go are implemented implicitly.
func (s Severity) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// Finding is one problem that was found.
type Finding struct {
	RuleID   string   `json:"rule"`
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
}

// Rule is the interface of a lint rule. To add a rule, create a type with
// these two methods and call Register in the init() of its file.
type Rule interface {
	ID() string
	Check(doc *Doc, repo *Repo) []Finding
}

// Describer is an optional interface: a short rule description for --list-rules.
type Describer interface {
	Description() string
}

var (
	registryMu sync.Mutex
	registry   = map[string]Rule{}
)

// Register adds a rule to the shared registry. A duplicate ID is a
// programmer error, hence the panic (it fires right at startup).
func Register(r Rule) {
	registryMu.Lock()
	defer registryMu.Unlock() // defer runs when the function returns
	id := r.ID()
	if _, dup := registry[id]; dup {
		panic(fmt.Sprintf("lint: rule %s registered twice", id))
	}
	registry[id] = r
}

// Rules returns all registered rules sorted by ID.
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

// F is a short Finding constructor for rules.
func F(rule string, sev Severity, doc *Doc, line int, msg, hint string) Finding {
	return Finding{RuleID: rule, Severity: sev, File: doc.Path, Line: line, Message: msg, Hint: hint}
}

// SortFindings sorts by file, line and rule ID.
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
