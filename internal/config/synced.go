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

// syncedMarker — комментарий над секцией, которую генерирует sync.
const syncedMarker = "# --- holdfast sync: секция генерируется автоматически, не редактируйте ---"

var headerRe = regexp.MustCompile(`^\s*\[`)
var syncedHeaderRe = regexp.MustCompile(`^\s*\[\[?\s*guard\.synced\b`)

// WriteSynced заменяет секцию [guard.synced] в holdfast.toml на rules,
// сохраняя всё остальное содержимое файла (включая комментарии).
//
// Мы правим файл как текст, а не «загрузить → изменить → сохранить»,
// потому что TOML-энкодер потерял бы комментарии и порядок ключей.
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

// StripSynced удаляет из текста конфига сгенерированную секцию guard.synced.
func StripSynced(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inSynced := false
	for _, line := range lines {
		if strings.TrimSpace(line) == syncedMarker {
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

// quote превращает строку в базовую строку TOML ("...").
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
