package rules

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/roma2246/holdfast/internal/lint"
)

func init() { lint.Register(duplicatesRule{}) }

type duplicatesRule struct{}

func (duplicatesRule) ID() string { return "HF005" }
func (duplicatesRule) Description() string {
	return "Дублирующиеся или почти одинаковые пункты"
}

// Порог похожести из ТЗ: > 90%.
const similarityThreshold = 0.9

// minWords — короткие строки («## Tests», «- Go») не сравниваем.
const minWords = 3

// Words разбивает строку на множество слов в нижнем регистре.
// map[string]struct{} — идиоматичное «множество» в Go: struct{} не
// занимает памяти, нам важны только ключи.
func Words(s string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
	}) {
		set[w] = struct{}{}
	}
	return set
}

// Jaccard — мера Жаккара: |A ∩ B| / |A ∪ B|.
func Jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for w := range a {
		if _, ok := b[w]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}

func (r duplicatesRule) Check(doc *lint.Doc, _ *lint.Repo) []lint.Finding {
	type item struct {
		line  int
		text  string
		words map[string]struct{}
	}
	var items []item
	for i, raw := range doc.Lines {
		n := i + 1
		text := strings.TrimSpace(lint.StripListMarker(raw))
		if doc.InCode(n) || doc.IsHeading(n) || strings.HasPrefix(text, "<!--") || strings.HasPrefix(text, "|") {
			continue
		}
		w := Words(text)
		if len(w) < minWords {
			continue
		}
		items = append(items, item{n, text, w})
	}

	var out []lint.Finding
	dup := map[int]bool{}
	for j := range items {
		for i := 0; i < j; i++ {
			if dup[items[j].line] {
				break
			}
			sim := Jaccard(items[i].words, items[j].words)
			if sim <= similarityThreshold {
				continue
			}
			dup[items[j].line] = true
			msg := fmt.Sprintf("дублирует строку %d", items[i].line)
			if sim < 1 {
				msg = fmt.Sprintf("почти повторяет строку %d (похожесть %.0f%%)", items[i].line, sim*100)
			}
			out = append(out, lint.F(r.ID(), lint.Warning, doc, items[j].line, msg,
				"удалите повтор — он тратит контекст и не добавляет смысла"))
		}
	}
	return out
}
