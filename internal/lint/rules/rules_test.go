package rules_test

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/roma2246/holdfast/internal/config"
	"github.com/roma2246/holdfast/internal/lint"
	"github.com/roma2246/holdfast/internal/lint/rules"
)

const fixtures = "../../../testdata/lint"

// runFixture прогоняет по папке-фикстуре только правило ruleID
// (или все правила, если ruleID пустой).
func runFixture(t *testing.T, dir, ruleID string) []lint.Finding {
	t.Helper() // в сообщении об ошибке будет строка вызывающего теста
	root, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := lint.Discover(root, cfg.Lint.Files)
	if err != nil || len(files) == 0 {
		t.Fatalf("нет файлов в %s: %v", dir, err)
	}
	var selected []lint.Rule
	for _, r := range lint.Rules() {
		if ruleID == "" || r.ID() == ruleID {
			selected = append(selected, r)
		}
	}
	fs, err := lint.Run(context.Background(), lint.NewRepo(root, cfg), files, selected)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// Для каждого правила: фикстура bad должна срабатывать, good — нет.
func TestRulesFixtures(t *testing.T) {
	ids := []string{"HF001", "HF002", "HF003", "HF004", "HF005", "HF006", "HF007", "HF008", "HF009"}
	if got := len(lint.Rules()); got != len(ids) {
		t.Fatalf("зарегистрировано правил: %d, ожидалось %d", got, len(ids))
	}
	for _, id := range ids {
		t.Run(id+"/bad", func(t *testing.T) {
			fs := runFixture(t, filepath.Join(fixtures, id, "bad"), id)
			if len(fs) == 0 {
				t.Fatalf("%s не сработало на bad-фикстуре", id)
			}
			for _, f := range fs {
				if f.RuleID != id || f.Line < 1 || f.Message == "" || f.Hint == "" {
					t.Errorf("некорректная находка: %+v", f)
				}
			}
		})
		t.Run(id+"/good", func(t *testing.T) {
			if fs := runFixture(t, filepath.Join(fixtures, id, "good"), id); len(fs) != 0 {
				t.Fatalf("%s ложно сработало: %+v", id, fs)
			}
		})
	}
}

// Точное содержимое находок на нескольких bad-фикстурах.
func TestRuleDetails(t *testing.T) {
	tests := []struct {
		id       string
		wantN    int
		contains []string
		lines    []int
	}{
		{"HF001", 3, []string{"src/api/", "docs/setup.md", "missing.go"}, []int{3, 3, 9}},
		{"HF002", 3, []string{"npm run e2e", "make deploy", "justfile не найден"}, []int{5, 6, 7}},
		{"HF004", 1, []string{"use yarn"}, []int{10}},
		{"HF005", 2, []string{"дублирует строку 5", "почти повторяет строку 5"}, []int{7, 8}},
		{"HF006", 2, []string{"AWS", "AKIA*"}, []int{5, 6}},
		{"HF007", 2, []string{"write good code", "пиши чисто"}, []int{3, 4}},
		{"HF009", 2, []string{"migrations/", "docker compose down"}, []int{5, 6}},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			fs := runFixture(t, filepath.Join(fixtures, tt.id, "bad"), tt.id)
			if len(fs) != tt.wantN {
				t.Fatalf("находок %d, ожидалось %d: %+v", len(fs), tt.wantN, fs)
			}
			all := ""
			for i, f := range fs {
				all += f.Message + "\n"
				if f.Line != tt.lines[i] {
					t.Errorf("находка %d: строка %d, ожидалась %d (%s)", i, f.Line, tt.lines[i], f.Message)
				}
			}
			for _, c := range tt.contains {
				if !strings.Contains(all, c) {
					t.Errorf("в сообщениях нет %q:\n%s", c, all)
				}
			}
		})
	}
}

// Общая фикстура должна давать все типы проблем сразу.
func TestAllRulesOnOneFile(t *testing.T) {
	fs := runFixture(t, filepath.Join(fixtures, "all"), "")
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.RuleID] = true
	}
	var got []string
	for id := range seen {
		got = append(got, id)
	}
	sort.Strings(got)
	want := "HF001 HF002 HF003 HF004 HF005 HF006 HF007 HF008 HF009"
	if strings.Join(got, " ") != want {
		t.Fatalf("сработали %v, ожидались %s", got, want)
	}
	// Вывод отсортирован по файлу и строке.
	for i := 1; i < len(fs); i++ {
		if fs[i].Line < fs[i-1].Line {
			t.Fatalf("находки не отсортированы: %+v", fs)
		}
	}
}

func TestDisableComment(t *testing.T) {
	text := "<!-- holdfast-disable HF007 -->\n# P\n\n- Write good code.\n<!-- holdfast-disable-next-line HF001 -->\nSee `nope/missing.md`.\nAlso `gone/file.md`.\n"
	doc := lint.Parse("/x/CLAUDE.md", "CLAUDE.md", text)
	repo := lint.NewRepo("/x", nil)
	fs := lint.CheckDoc(doc, repo, lint.Rules())
	for _, f := range fs {
		if f.RuleID == "HF007" || (f.RuleID == "HF001" && f.Line == 6) {
			t.Errorf("правило должно быть выключено: %+v", f)
		}
	}
	found := false
	for _, f := range fs {
		if f.RuleID == "HF001" && f.Line == 7 {
			found = true
		}
	}
	if !found {
		t.Errorf("HF001 на строке 7 должно остаться: %+v", fs)
	}
}

func TestDisableInConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Lint.Disable = []string{"hf007"}
	doc := lint.Parse("/x/CLAUDE.md", "CLAUDE.md", "# P\n## Commands\n- Write good code.\n")
	for _, f := range lint.CheckDoc(doc, lint.NewRepo("/x", cfg), lint.Rules()) {
		if f.RuleID == "HF007" {
			t.Errorf("HF007 выключено в конфиге: %+v", f)
		}
	}
}

func TestJaccard(t *testing.T) {
	tests := []struct {
		a, b string
		want float64
	}{
		{"use go modules", "use go modules", 1},
		{"a b c d", "a b c e", 0.6},
		{"", "", 1},
		{"one two", "three four", 0},
	}
	for _, tt := range tests {
		if got := rules.Jaccard(rules.Words(tt.a), rules.Words(tt.b)); got != tt.want {
			t.Errorf("Jaccard(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := rules.EstimateTokens("привет, мир!"); got != 3 { // 12 символов / 4
		t.Errorf("EstimateTokens = %d, want 3", got)
	}
}

func TestHF008WithoutHeadings(t *testing.T) {
	doc := lint.Parse("/x/.cursorrules", ".cursorrules", "Always run `npm test` before finishing.\n")
	for _, f := range lint.CheckDoc(doc, lint.NewRepo("/x", nil), lint.Rules()) {
		if f.RuleID == "HF008" {
			t.Errorf("HF008 не должно срабатывать, есть команда теста: %+v", f)
		}
	}
}
