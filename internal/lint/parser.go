package lint

import (
	"os"
	"regexp"
	"strings"
)

// Section — заголовок markdown и диапазон строк под ним.
type Section struct {
	Title     string
	Level     int // количество '#'
	StartLine int // 1-based, строка заголовка
	EndLine   int // последняя строка секции включительно
}

// Doc — разобранный файл инструкций агента.
type Doc struct {
	Path     string   // путь относительно корня репозитория (для вывода)
	AbsPath  string   // абсолютный путь
	Dir      string   // абсолютная папка файла
	Text     string   // весь текст
	Lines    []string // строки без '\n'; Lines[0] — строка 1
	Sections []Section

	inCode       []bool          // inCode[i] — строка i+1 внутри ```блока```
	disabledFile map[string]bool // правила, выключенные на весь файл
	disabledLine map[int]map[string]bool
}

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	fenceRe   = regexp.MustCompile("^\\s*(```|~~~)")
	// <!-- holdfast-disable HF003 HF005 --> и
	// <!-- holdfast-disable-next-line HF001 -->
	disableRe = regexp.MustCompile(`<!--\s*holdfast-disable(-next-line)?\s+([A-Za-z0-9,\s]+?)\s*-->`)
)

// ParseFile читает и разбирает файл.
func ParseFile(absPath, relPath string) (*Doc, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	return Parse(absPath, relPath, string(data)), nil
}

// Parse разбирает текст markdown: секции, блоки кода, комментарии-выключатели.
func Parse(absPath, relPath, text string) *Doc {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	// Файл, оканчивающийся на '\n', даёт лишнюю пустую строку в конце.
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	d := &Doc{
		Path:         relPath,
		AbsPath:      absPath,
		Dir:          dirOf(absPath),
		Text:         text,
		Lines:        lines,
		inCode:       make([]bool, len(lines)),
		disabledFile: map[string]bool{},
		disabledLine: map[int]map[string]bool{},
	}

	inFence := false
	for i, line := range lines {
		if fenceRe.MatchString(line) {
			d.inCode[i] = true
			inFence = !inFence
			continue
		}
		d.inCode[i] = inFence
		if inFence {
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			// Закрываем предыдущую секцию.
			if n := len(d.Sections); n > 0 {
				d.Sections[n-1].EndLine = i
			}
			d.Sections = append(d.Sections, Section{Title: m[2], Level: len(m[1]), StartLine: i + 1})
		}
		for _, m := range disableRe.FindAllStringSubmatch(line, -1) {
			ids := strings.FieldsFunc(strings.ToUpper(m[2]), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
			for _, id := range ids {
				if m[1] != "" { // -next-line
					if d.disabledLine[i+2] == nil {
						d.disabledLine[i+2] = map[string]bool{}
					}
					d.disabledLine[i+2][id] = true
				} else {
					d.disabledFile[id] = true
				}
			}
		}
	}
	if n := len(d.Sections); n > 0 {
		d.Sections[n-1].EndLine = len(lines)
	}
	return d
}

// InCode сообщает, лежит ли строка (1-based) внутри fenced-блока кода.
func (d *Doc) InCode(line int) bool {
	if line < 1 || line > len(d.inCode) {
		return false
	}
	return d.inCode[line-1]
}

// IsDisabled — выключено ли правило для данной строки комментарием.
func (d *Doc) IsDisabled(ruleID string, line int) bool {
	return d.disabledFile[ruleID] || d.disabledLine[line][ruleID]
}

// IsHeading — является ли строка заголовком (вне блока кода).
func (d *Doc) IsHeading(line int) bool {
	return !d.InCode(line) && line >= 1 && line <= len(d.Lines) && headingRe.MatchString(d.Lines[line-1])
}

var inlineCodeRe = regexp.MustCompile("`([^`]+)`")

// InlineCode возвращает фрагменты `в обратных кавычках` строки.
func InlineCode(line string) []string {
	var out []string
	for _, m := range inlineCodeRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

// StripInlineCode заменяет `код` пробелами (длина строки сохраняется).
func StripInlineCode(line string) string {
	return inlineCodeRe.ReplaceAllStringFunc(line, func(s string) string {
		return strings.Repeat(" ", len(s))
	})
}

var listMarkerRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?`)

// StripListMarker убирает «- », «1. », «- [ ] » в начале строки.
func StripListMarker(line string) string {
	return listMarkerRe.ReplaceAllString(line, "")
}

func dirOf(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "."
	}
	if i == 0 {
		return p[:1]
	}
	return p[:i]
}
