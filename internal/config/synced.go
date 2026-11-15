package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// syncedMarker is the comment above the section that sync generates.
const syncedMarker = "# --- holdfast sync: this section is generated automatically, do not edit ---"

// legacySyncedMarker is the same comment as written by versions up to 0.3
// (in Russian). It is still recognized so that a re-sync cleans it up.
const legacySyncedMarker = "# --- holdfast sync: секция генерируется автоматически, не редактируйте ---"

var headerRe = regexp.MustCompile(`^\s*\[`)
var syncedHeaderRe = regexp.MustCompile(`^\s*\[\[?\s*guard\.synced\b`)

// WriteSynced replaces the [guard.synced] section in holdfast.toml with rules,
// keeping the rest of the file's contents (including comments).
//
// We edit the file as text rather than "load → modify → save", because
// a TOML encoder would lose the comments and the key order.
func WriteSynced(root string, rules []SyncedRule) error {
	path := filepath.Join(root, FileName)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	text := StripSynced(string(data))
	if text == "" {
		text = Template
	}
	text = strings.TrimRight(text, "\n") + "\n"
	if len(rules) > 0 {
		var b strings.Builder
		b.WriteString("\n" + syncedMarker + "\n")
		for _, r := range rules {
			b.WriteString("[[guard.synced.rules]]\n")
			fmt.Fprintf(&b, "kind = %s\n", quote(r.Kind))
			fmt.Fprintf(&b, "pattern = %s\n", quote(r.Pattern))
			fmt.Fprintf(&b, "source = %s\n", quote(r.Source))
			fmt.Fprintf(&b, "origin = %s\n\n", quote(r.Origin))
		}
		text += strings.TrimRight(b.String(), "\n") + "\n"
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

// StripSynced removes the generated guard.synced section from the config text.
func StripSynced(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inSynced := false
	for _, line := range lines {
		if t := strings.TrimSpace(line); t == syncedMarker || t == legacySyncedMarker {
			continue
		}
		if headerRe.MatchString(line) {
			inSynced = syncedHeaderRe.MatchString(line)
		}
		if !inSynced {
			out = append(out, line)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

// quote turns a string into a TOML basic string ("...").
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
