package lint

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/charmbracelet/lipgloss"
)

// Summary — количество находок по уровням.
type Summary struct {
	Files    int `json:"files"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos"`
}

// Summarize считает находки по уровням.
func Summarize(files int, fs []Finding) Summary {
	s := Summary{Files: files}
	for _, f := range fs {
		switch f.Severity {
		case Error:
			s.Errors++
		case Warning:
			s.Warnings++
		default:
			s.Infos++
		}
	}
	return s
}

// WriteJSON печатает отчёт для CI.
func WriteJSON(w io.Writer, files int, fs []Finding) error {
	if fs == nil {
		fs = []Finding{} // чтобы в JSON был [], а не null
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Findings []Finding `json:"findings"`
		Summary  Summary   `json:"summary"`
	}{fs, Summarize(files, fs)})
}

// WriteHuman печатает цветной отчёт. Цвета включаются, только если w —
// терминал: lipgloss.NewRenderer сам определяет возможности вывода.
func WriteHuman(w io.Writer, files int, fs []Finding) {
	r := lipgloss.NewRenderer(w)
	var (
		fileStyle = r.NewStyle().Bold(true)
		ruleStyle = r.NewStyle().Foreground(lipgloss.Color("12"))
		hintStyle = r.NewStyle().Faint(true)
		sevStyle  = map[Severity]lipgloss.Style{
			Error:   r.NewStyle().Foreground(lipgloss.Color("9")).Bold(true),
			Warning: r.NewStyle().Foreground(lipgloss.Color("11")),
			Info:    r.NewStyle().Foreground(lipgloss.Color("14")),
		}
		okStyle = r.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	)

	for _, f := range fs {
		loc := fmt.Sprintf("%s:%d", f.File, f.Line)
		// Width выравнивает колонку уровня, как в примере из ТЗ:
		// CLAUDE.md:14  HF002 error  «npm run e2e» — ...
		fmt.Fprintf(w, "%s  %s %s  %s\n",
			fileStyle.Render(loc),
			ruleStyle.Render(f.RuleID),
			sevStyle[f.Severity].Width(7).Render(f.Severity.String()),
			f.Message)
		if f.Hint != "" {
			fmt.Fprintf(w, "    %s\n", hintStyle.Render("↳ "+f.Hint))
		}
	}

	s := Summarize(files, fs)
	if len(fs) > 0 {
		fmt.Fprintln(w)
	}
	if s.Errors+s.Warnings+s.Infos == 0 {
		fmt.Fprintln(w, okStyle.Render(fmt.Sprintf("✓ Files checked: %d, no problems found", files)))
		return
	}
	fmt.Fprintf(w, "Files checked: %d · %s · %s · %s\n", files,
		sevStyle[Error].Render(fmt.Sprintf("errors: %d", s.Errors)),
		sevStyle[Warning].Render(fmt.Sprintf("warnings: %d", s.Warnings)),
		sevStyle[Info].Render(fmt.Sprintf("notes: %d", s.Infos)))
}
