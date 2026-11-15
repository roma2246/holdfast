package rules

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(contradictionRule{}) }

type contradictionRule struct{}

func (contradictionRule) ID() string { return "HF004" }
func (contradictionRule) Description() string {
	return "Contradiction: the same thing under \"always/use\" and \"never/don't\""
}

const (
	enVerbs = `use|using|run|edit|modify|commit|install|add|call|import|touch|delete|create|prefer`
	ruVerbs = `используй(?:те)?|запускай(?:те)?|редактируй(?:те)?|меняй(?:те)?|трогай(?:те)?|коммить(?:те)?|устанавливай(?:те)?|удаляй(?:те)?|добавляй(?:те)?`
	objPat  = `(?:(?:the|a|an)\s+)?([\p{L}\p{N}_.\-/@:+#]+)`
)

var (
	negRes = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:never|don'?t|do not|must not|should not|shouldn'?t|cannot|can'?t)\s+(` + enVerbs + `)\s+` + objPat),
		regexp.MustCompile(`(?:^|[^\p{L}])(?:никогда\s+)?не\s+(` + ruVerbs + `)\s+` + objPat),
	}
	avoidRe = regexp.MustCompile(`\bavoid\s+(?:using\s+)?` + objPat)
	posRes  = []*regexp.Regexp{
		regexp.MustCompile(`\b(` + enVerbs + `)\s+` + objPat),
		regexp.MustCompile(`(?:^|[^\p{L}])(` + ruVerbs + `)\s+` + objPat),
	}
	verbNorm = map[string]string{
		"using": "use", "prefer": "use", "используй": "use",
		"запускай": "run", "modify": "edit", "touch": "edit", "трогай": "edit",
		"редактируй": "edit", "меняй": "edit", "коммить": "commit",
		"устанавливай": "install", "удаляй": "delete", "добавляй": "add",
	}
	// Words after which the object is meaningless for comparison.
	stopObjects = map[string]bool{"it": true, "this": true, "that": true, "them": true, "это": true, "его": true, "их": true}
)

type directive struct {
	key  string // "use yarn"
	text string // as written
	line int
}

func normVerb(v string) string {
	v = strings.TrimSuffix(v, "те")
	if n, ok := verbNorm[v]; ok {
		return n
	}
	return v
}

func normObj(o string) string {
	return strings.Trim(o, ".,;:!?\"'«»()[]`")
}

func (r contradictionRule) Check(doc *lint.Doc, _ *lint.Repo) []lint.Finding {
	var pos, neg []directive
	for i, raw := range doc.Lines {
		n := i + 1
		if doc.InCode(n) || doc.IsHeading(n) {
			continue
		}
		line := strings.ToLower(strings.ReplaceAll(raw, "`", ""))
		// First find the prohibitions and "cut" them out of the line, otherwise
		// "never use yarn" would also be counted as "use yarn".
		masked := line
		mask := func(loc []int) {
			masked = masked[:loc[0]] + strings.Repeat(" ", loc[1]-loc[0]) + masked[loc[1]:]
		}
		for _, re := range negRes {
			for _, m := range re.FindAllStringSubmatchIndex(line, -1) {
				verb, obj := normVerb(line[m[2]:m[3]]), normObj(line[m[4]:m[5]])
				if obj != "" && !stopObjects[obj] {
					neg = append(neg, directive{verb + " " + obj, strings.TrimSpace(line[m[0]:m[1]]), n})
				}
				mask(m[:2])
			}
		}
		for _, m := range avoidRe.FindAllStringSubmatchIndex(line, -1) {
			obj := normObj(line[m[2]:m[3]])
			if obj != "" && !stopObjects[obj] {
				neg = append(neg, directive{"use " + obj, strings.TrimSpace(line[m[0]:m[1]]), n})
			}
			mask(m[:2])
		}
		for _, re := range posRes {
			for _, m := range re.FindAllStringSubmatchIndex(masked, -1) {
				verb, obj := normVerb(masked[m[2]:m[3]]), normObj(masked[m[4]:m[5]])
				if obj != "" && !stopObjects[obj] {
					pos = append(pos, directive{verb + " " + obj, strings.TrimSpace(masked[m[0]:m[1]]), n})
				}
			}
		}
	}

	var out []lint.Finding
	reported := map[string]bool{}
	for _, nd := range neg {
		for _, pd := range pos {
			if pd.key != nd.key || reported[nd.key] {
				continue
			}
			reported[nd.key] = true
			// Report the later of the two lines.
			first, second := pd, nd
			if second.line < first.line {
				first, second = second, first
			}
			out = append(out, lint.F(r.ID(), lint.Warning, doc, second.line,
				fmt.Sprintf("contradiction: \"%s\" (line %d) and \"%s\"", first.text, first.line, second.text),
				"keep one rule: the agent doesn't know which one to follow"))
		}
	}
	return out
}
