// Package bridge превращает запреты из CLAUDE.md в политики guard.
//
// Два источника правил:
//   - явный блок <!-- holdfast deny-paths: ... ask-commands: ... --> —
//     надёжный, применяется сразу;
//   - эвристики по тексту («never edit X», «не трогай X») — требуют
//     подтверждения пользователя.
package bridge

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/roma2246/holdfast/internal/config"
)

// Происхождение правила.
const (
	OriginExplicit  = "explicit"
	OriginHeuristic = "heuristic"
)

// Rule — правило, извлечённое из файла инструкций.
type Rule struct {
	Kind    string // config.KindDenyPath, ...
	Pattern string
	File    string
	Line    int
	Origin  string
	Text    string // исходная фраза (для показа пользователю)
}

// Source — «CLAUDE.md:12».
func (r Rule) Source() string { return fmt.Sprintf("%s:%d", r.File, r.Line) }

// ToSynced превращает правило в запись секции [guard.synced].
func (r Rule) ToSynced() config.SyncedRule {
	return config.SyncedRule{Kind: r.Kind, Pattern: r.Pattern, Source: r.Source(), Origin: r.Origin}
}

// IsPath — правило про пути (иначе про команды).
func (r Rule) IsPath() bool {
	return r.Kind == config.KindDenyPath || r.Kind == config.KindAskPath
}

// Extract возвращает явные и эвристические правила файла.
func Extract(file string, lines []string) []Rule {
	return append(ExtractExplicit(file, lines), ExtractHeuristic(file, lines)...)
}

var (
	blockStartRe = regexp.MustCompile(`<!--\s*holdfast(?:\s|$)`)
	blockKeyRe   = regexp.MustCompile(`\b(deny|ask)-(paths?|commands?)\s*:`)
)

// ExtractExplicit разбирает блоки <!-- holdfast ... -->. Блок может быть
// однострочным или многострочным; ключи: deny-paths, ask-paths,
// deny-commands, ask-commands; значения через запятую.
func ExtractExplicit(file string, lines []string) []Rule {
	var out []Rule
	for i := 0; i < len(lines); i++ {
		loc := blockStartRe.FindStringIndex(lines[i])
		if loc == nil {
			continue
		}
		// Собираем текст блока до "-->", запоминая, с какой строки
		// начинается каждый фрагмент, чтобы знать номер строки правила.
		type chunk struct {
			text string
			line int
		}
		var chunks []chunk
		rest := lines[i][loc[1]:]
		j := i
		for {
			if k := strings.Index(rest, "-->"); k >= 0 {
				chunks = append(chunks, chunk{rest[:k], j + 1})
				break
			}
			chunks = append(chunks, chunk{rest, j + 1})
			j++
			if j >= len(lines) {
				break
			}
			rest = lines[j]
		}
		i = j

		kind := ""
		for _, c := range chunks {
			keys := blockKeyRe.FindAllStringSubmatchIndex(c.text, -1)
			// Текст до первого ключа строки продолжает предыдущий ключ.
			segStart := 0
			for ki := 0; ki <= len(keys); ki++ {
				segEnd := len(c.text)
				if ki < len(keys) {
					segEnd = keys[ki][0]
				}
				if kind != "" {
					for _, v := range strings.Split(c.text[segStart:segEnd], ",") {
						if v = strings.TrimSpace(v); v != "" {
							out = append(out, Rule{Kind: kind, Pattern: normalize(kind, v), File: file,
								Line: c.line, Origin: OriginExplicit, Text: v})
						}
					}
				}
				if ki < len(keys) {
					k := keys[ki]
					kind = keyKind(c.text[k[2]:k[3]], c.text[k[4]:k[5]])
					segStart = k[1]
				}
			}
		}
	}
	return out
}

func keyKind(action, object string) string {
	isPath := strings.HasPrefix(object, "path")
	switch {
	case action == "deny" && isPath:
		return config.KindDenyPath
	case action == "ask" && isPath:
		return config.KindAskPath
	case action == "deny":
		return config.KindDenyCommand
	default:
		return config.KindAskCommand
	}
}

const target = "(?:`([^`]+)`|([A-Za-z0-9_.\\-*/]+))"

var (
	pathHeuristics = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:never|don'?t|do not|must not|should not)\s+(?:edit|modify|touch|change|delete|remove|rewrite|write to|commit to)\s+(?:(?:the|any|files?|anything)\s+)*(?:(?:in|inside|under)\s+)?(?:the\s+)?` + target),
		regexp.MustCompile(`(?i)(?:никогда\s+)?не\s+(?:трогай|редактируй|меняй|изменяй|удаляй|правь)(?:те)?\s+(?:(?:файлы|папку|директорию|ничего)\s+)?(?:в\s+)?` + target),
	}
	cmdHeuristics = []*regexp.Regexp{
		regexp.MustCompile("(?i)\\b(?:never|don'?t|do not|must not|should not)\\s+(?:run|execute|call)\\s+`([^`]+)`"),
		regexp.MustCompile("(?i)(?:никогда\\s+)?не\\s+(?:запускай|выполняй|вызывай)(?:те)?\\s+`([^`]+)`"),
	}
	fenceRe = regexp.MustCompile("^\\s*(```|~~~)")
)

// ExtractHeuristic ищет фразы-запреты в обычном тексте (вне блоков кода).
func ExtractHeuristic(file string, lines []string) []Rule {
	var out []Rule
	inFence := false
	for i, line := range lines {
		if fenceRe.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence || strings.Contains(line, "<!--") {
			continue
		}
		for _, re := range pathHeuristics {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				t := m[1]
				if t == "" {
					t = m[2]
				}
				t = strings.TrimRight(t, ".,;:")
				if !looksLikePath(t) {
					continue
				}
				out = append(out, Rule{Kind: config.KindDenyPath, Pattern: normalize(config.KindDenyPath, t),
					File: file, Line: i + 1, Origin: OriginHeuristic, Text: strings.TrimSpace(m[0])})
			}
		}
		for _, re := range cmdHeuristics {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				out = append(out, Rule{Kind: config.KindDenyCommand, Pattern: strings.TrimSpace(m[1]),
					File: file, Line: i + 1, Origin: OriginHeuristic, Text: strings.TrimSpace(m[0])})
			}
		}
	}
	return out
}

// looksLikePath отсекает «never edit code» — нужен '/', '.', или '*'.
func looksLikePath(s string) bool {
	return s != "" && !strings.Contains(s, " ") && strings.ContainsAny(s, "/.*") && s != "." && s != ".."
}

// normalize приводит путь к glob: "./migrations/" → "migrations/**".
func normalize(kind, v string) string {
	v = strings.TrimSpace(v)
	if kind != config.KindDenyPath && kind != config.KindAskPath {
		return v
	}
	v = strings.TrimPrefix(v, "./")
	if strings.HasSuffix(v, "/") {
		v += "**"
	}
	return v
}

// Covered — есть ли такое правило в политике guard (с учётом синхронизированных).
func Covered(r Rule, g *config.GuardConfig) bool {
	for _, s := range g.Synced.Rules {
		if s.Pattern == r.Pattern && (r.IsPath() == (s.Kind == config.KindDenyPath || s.Kind == config.KindAskPath)) {
			return true
		}
	}
	list := g.DenyCommands
	if r.IsPath() {
		list = g.ProtectedPaths
	} else {
		list = append(append([]string(nil), list...), g.AskCommands...)
	}
	for _, p := range list {
		if normalize(r.Kind, p) == r.Pattern {
			return true
		}
	}
	return false
}

// Dedupe убирает повторы (одинаковые вид+шаблон), оставляя первое
// вхождение; явные правила важнее эвристических.
func Dedupe(rules []Rule) []Rule {
	seen := map[string]bool{}
	var out []Rule
	for _, pass := range []string{OriginExplicit, OriginHeuristic} {
		for _, r := range rules {
			key := r.Kind + "\x00" + r.Pattern
			if r.Origin != pass || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}
