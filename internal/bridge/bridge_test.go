package bridge

import (
	"strconv"
	"strings"
	"testing"

	"github.com/roma2246/holdfast/internal/config"
)

func lines(s string) []string { return strings.Split(s, "\n") }

func TestExtractExplicit(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string // "kind pattern line"
	}{
		{
			"однострочный блок из ТЗ",
			"# P\n<!-- holdfast deny-paths: migrations/**, .github/workflows/** ask-commands: docker compose down -->",
			[]string{"deny-path migrations/** 2", "deny-path .github/workflows/** 2", "ask-command docker compose down 2"},
		},
		{
			"многострочный блок",
			"<!-- holdfast\ndeny-paths: ./db/, schema.sql\nask-paths: docs/**\ndeny-commands: terraform destroy\n-->\ntext",
			[]string{"deny-path db/** 2", "deny-path schema.sql 2", "ask-path docs/** 3", "deny-command terraform destroy 4"},
		},
		{
			"значения на следующей строке",
			"<!-- holdfast\ndeny-paths:\n  a/**,\n  b/**\n-->",
			[]string{"deny-path a/** 3", "deny-path b/** 4"},
		},
		{
			"выключатель lint — не блок правил",
			"<!-- holdfast-disable HF003 -->",
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range ExtractExplicit("CLAUDE.md", lines(tt.text)) {
				if r.Origin != OriginExplicit {
					t.Errorf("origin = %s", r.Origin)
				}
				got = append(got, r.Kind+" "+r.Pattern+" "+strconv.Itoa(r.Line))
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestExtractHeuristic(t *testing.T) {
	tests := []struct {
		line string
		want string // "kind pattern" или "" — ничего
	}{
		{"- Never edit `migrations/` by hand.", "deny-path migrations/**"},
		{"Don't touch the generated/ folder", "deny-path generated/**"},
		{"Do not modify files in `src/legacy/**`", "deny-path src/legacy/**"},
		{"- Не трогай `vendor/`", "deny-path vendor/**"},
		{"Никогда не редактируй package-lock.json", "deny-path package-lock.json"},
		{"Never run `make deploy` locally.", "deny-command make deploy"},
		{"Не запускай `docker compose down`", "deny-command docker compose down"},
		{"Never edit code without tests", ""}, // «code» — не путь
		{"Never run tests in parallel", ""},   // без `кода` — не команда
		{"Always edit `src/`", ""},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			rules := ExtractHeuristic("CLAUDE.md", []string{tt.line})
			got := ""
			if len(rules) > 0 {
				got = rules[0].Kind + " " + rules[0].Pattern
				if rules[0].Origin != OriginHeuristic || rules[0].Text == "" || rules[0].Source() != "CLAUDE.md:1" {
					t.Errorf("rule: %+v", rules[0])
				}
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHeuristicSkipsCodeBlocks(t *testing.T) {
	text := "```\nnever edit `x/`\n```\n<!-- never edit `y/` -->"
	if r := ExtractHeuristic("f", lines(text)); len(r) != 0 {
		t.Errorf("в блоках кода и комментариях правил быть не должно: %+v", r)
	}
}

func TestCoveredAndDedupe(t *testing.T) {
	g := &config.GuardConfig{
		ProtectedPaths: []string{"./migrations/"},
		DenyCommands:   []string{"make deploy"},
		AskCommands:    []string{"docker compose down"},
		Synced:         config.SyncedConfig{Rules: []config.SyncedRule{{Kind: config.KindDenyPath, Pattern: "vendor/**"}}},
	}
	tests := []struct {
		r    Rule
		want bool
	}{
		{Rule{Kind: config.KindDenyPath, Pattern: "migrations/**"}, true},
		{Rule{Kind: config.KindDenyPath, Pattern: "vendor/**"}, true},
		{Rule{Kind: config.KindDenyPath, Pattern: "other/**"}, false},
		{Rule{Kind: config.KindDenyCommand, Pattern: "make deploy"}, true},
		{Rule{Kind: config.KindDenyCommand, Pattern: "docker compose down"}, true},
		{Rule{Kind: config.KindDenyCommand, Pattern: "vendor/**"}, false},
	}
	for _, tt := range tests {
		if got := Covered(tt.r, g); got != tt.want {
			t.Errorf("Covered(%+v) = %v", tt.r, got)
		}
	}

	in := []Rule{
		{Kind: config.KindDenyPath, Pattern: "a/**", Origin: OriginHeuristic, Line: 1},
		{Kind: config.KindDenyPath, Pattern: "a/**", Origin: OriginExplicit, Line: 2},
		{Kind: config.KindDenyPath, Pattern: "b/**", Origin: OriginHeuristic, Line: 3},
	}
	out := Dedupe(in)
	if len(out) != 2 || out[0].Origin != OriginExplicit || out[1].Pattern != "b/**" {
		t.Errorf("Dedupe: %+v", out)
	}
	if s := out[0].ToSynced(); s.Source != ":2" || s.Kind != config.KindDenyPath {
		t.Errorf("ToSynced: %+v", s)
	}
}
