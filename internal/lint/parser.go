package lint

import (
	"os"
	"regexp"
	"strings"
)

// Section is a markdown heading and the range of lines under it.
type Section struct {
	Title     string
	Level     int // number of '#'
	StartLine int // 1-based, the heading line
	EndLine   int // the last line of the section, inclusive
}

// Doc is a parsed agent instructions file.
type Doc struct {
	Path     string   // path relative to the repository root (for output)
	AbsPath  string   // absolute path
	Dir      string   // absolute directory of the file
	Text     string   // the whole text
	Lines    []string // lines without '\n'; Lines[0] is line 1
	Sections []Section

	inCode       []bool          // inCode[i] means line i+1 is inside a ```block```
	disabledFile map[string]bool // rules disabled for the whole file
	disabledLine map[int]map[string]bool
}

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	fenceRe   = regexp.MustCompile("^\\s*(```|~~~)")
	// <!-- holdfast-disable HF003 HF005 --> and
	// <!-- holdfast-disable-next-line HF001 -->
	disableRe = regexp.MustCompile(`<!--\s*holdfast-disable(-next-line)?\s+([A-Za-z0-9,\s]+?)\s*-->`)
)

// ParseFile reads and parses a file.
func ParseFile(absPath, relPath string) (*Doc, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	return Parse(absPath, relPath, string(data)), nil
}

// Parse parses markdown text: sections, code blocks, disabling comments.
func Parse(absPath, relPath, text string) *Doc {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	// A file ending in '\n' produces an extra empty line at the end.
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
			// Close the previous section.
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

// InCode reports whether a line (1-based) lies inside a fenced code block.
func (d *Doc) InCode(line int) bool {
	if line < 1 || line > len(d.inCode) {
		return false
	}
	return d.inCode[line-1]
}

// IsDisabled reports whether a comment disables the rule for the given line.
func (d *Doc) IsDisabled(ruleID string, line int) bool {
	return d.disabledFile[ruleID] || d.disabledLine[line][ruleID]
}

// IsHeading reports whether the line is a heading (outside a code block).
func (d *Doc) IsHeading(line int) bool {
	return !d.InCode(line) && line >= 1 && line <= len(d.Lines) && headingRe.MatchString(d.Lines[line-1])
}

var inlineCodeRe = regexp.MustCompile("`([^`]+)`")

// InlineCode returns the `backquoted` fragments of a line.
func InlineCode(line string) []string {
	var out []string
	for _, m := range inlineCodeRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

// StripInlineCode replaces `code` with spaces (the line length is preserved).
func StripInlineCode(line string) string {
	return inlineCodeRe.ReplaceAllStringFunc(line, func(s string) string {
		return strings.Repeat(" ", len(s))
	})
}

var listMarkerRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?`)

// StripListMarker removes "- ", "1. ", "- [ ] " at the start of a line.
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
