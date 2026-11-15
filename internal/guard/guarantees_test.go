package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roma2246/holdfast/internal/config"
)

// Тесты этого файла проверяют не отдельные примеры, а свойства, которые
// Holdfast обещает пользователю (по итогам внешнего ревью):
//
//   - всё, что откат перезаписал или удалил, сохранено в резервной копии —
//     либо откат не начинается;
//   - снимок другого рабочего дерева не выбирается;
//   - массовое удаление разрешается, только если удаляемое есть в точке;
//   - неудачная точка не выглядит как успешная;
//   - ошибка конфига не ослабляет защиту;
//   - дочерний shell не меняет контекст родителя;
//   - эквивалентные формы команды получают одинаковое решение: правила
//     путей применяются к итоговым файлам, push — к итоговой ветке;
//   - служебные файлы Holdfast агент не меняет ни одной формой.

// diskState читает все файлы рабочего дерева (включая игнорируемые), кроме .git.
func diskState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRollbackNeverLosesData(t *testing.T) {
	scenarios := []struct {
		name string
		// before — состояние на момент точки; after — что произошло потом.
		before, after func(t *testing.T, dir string)
	}{
		{
			name:   "обычные правки, новый и удалённый файл",
			before: func(t *testing.T, dir string) { write(t, dir, "keep.txt", "v1\n") },
			after: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "agent\n")
				write(t, dir, "new/file.txt", "new\n")
				os.Remove(filepath.Join(dir, "b.txt"))
			},
		},
		{
			// Сценарий из ревью: файл попал в .gitignore уже после точки.
			name:   "файл стал игнорируемым и изменился",
			before: func(t *testing.T, dir string) { write(t, dir, "local.txt", "v1\n") },
			after: func(t *testing.T, dir string) {
				write(t, dir, ".gitignore", "*.log\nlocal.txt\n")
				write(t, dir, "local.txt", "v2-precious\n")
			},
		},
		{
			name:   "на месте файла — папка с игнорируемым содержимым",
			before: func(t *testing.T, dir string) { write(t, dir, "data", "file\n") },
			after: func(t *testing.T, dir string) {
				os.Remove(filepath.Join(dir, "data"))
				write(t, dir, "data/cache.log", "ignored but precious\n")
				write(t, dir, "data/notes.txt", "tracked-ish\n")
			},
		},
		{
			name:   "на месте папки — игнорируемый файл",
			before: func(t *testing.T, dir string) { write(t, dir, "conf/app.txt", "v1\n") },
			after: func(t *testing.T, dir string) {
				os.RemoveAll(filepath.Join(dir, "conf"))
				write(t, dir, ".gitignore", "*.log\nconf\n")
				write(t, dir, "conf", "теперь это файл\n")
			},
		},
		{
			name:   "игнорируемый файл, которого не было в точке, не трогается",
			before: func(t *testing.T, dir string) {},
			after: func(t *testing.T, dir string) {
				write(t, dir, "debug.log", "мой журнал\n")
				write(t, dir, "a.txt", "agent\n")
			},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			dir := gitRepo(t, true)
			sc.before(t, dir)
			g, err := OpenGit(dir)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := g.CreateSnapshot(time.Now(), SnapshotMeta{})
			if err != nil {
				t.Fatal(err)
			}
			atSnapshot := diskState(t, dir)
			sc.after(t, dir)
			beforeRollback := diskState(t, dir)

			_, backup, err := g.Rollback(snap, time.Now())
			if err != nil {
				t.Fatalf("откат: %v", err)
			}
			afterRollback := diskState(t, dir)

			// Свойство 1: каждый изменённый или исчезнувший при откате файл
			// сохранён в резервной копии с прежним содержимым.
			for path, old := range beforeRollback {
				if now, ok := afterRollback[path]; ok && now == old {
					continue
				}
				saved, err := g.run(nil, nil, "show", backup.Ref+":"+path)
				if err != nil || saved != old {
					t.Errorf("ПОТЕРЯ ДАННЫХ: %s изменён откатом, а в резервной копии его нет (%v)", path, err)
				}
			}
			// Свойство 2: всё, что было в точке, вернулось.
			saved, err := g.TreeFiles(snap.Tree, nil)
			if err != nil {
				t.Fatal(err)
			}
			for path := range saved {
				if afterRollback[path] != atSnapshot[path] {
					t.Errorf("%s не восстановлен: %q, в точке было %q", path, afterRollback[path], atSnapshot[path])
				}
			}
			// Свойство 3: откат можно отменить штатной командой.
			undo, err := g.FindSnapshot(backup.Name())
			if err != nil {
				t.Fatalf("резервная копия не находится по имени: %v", err)
			}
			if _, _, err := g.Rollback(undo, time.Now()); err != nil {
				t.Fatalf("отмена отката: %v", err)
			}
			for path, old := range beforeRollback {
				if got := diskState(t, dir)[path]; got != old {
					t.Errorf("после отмены отката %s = %q, было %q", path, got, old)
				}
			}
		})
	}
}

// Если сохранить перезаписываемое нельзя, откат не начинается.
func TestRollbackStopsWhenBackupImpossible(t *testing.T) {
	dir := gitRepo(t, true)
	write(t, dir, "vendor", "file\n")
	g, _ := OpenGit(dir)
	snap, err := g.CreateSnapshot(time.Now(), SnapshotMeta{})
	if err != nil {
		t.Fatal(err)
	}
	// На месте файла — вложенный git-репозиторий: git не сохранит его содержимое.
	os.Remove(filepath.Join(dir, "vendor"))
	write(t, dir, "vendor/lib.txt", "чужой код\n")
	git(t, filepath.Join(dir, "vendor"), "init", "-q")
	git(t, filepath.Join(dir, "vendor"), "add", "-A")
	git(t, filepath.Join(dir, "vendor"), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "nested")
	before := diskState(t, dir)

	if _, _, err := g.Rollback(snap, time.Now()); err == nil || !strings.Contains(err.Error(), "откат отменён") {
		t.Fatalf("ожидался отказ от отката, получено: %v", err)
	}
	after := diskState(t, dir)
	if len(before) != len(after) || after["vendor/lib.txt"] != "чужой код\n" {
		t.Fatalf("после отказа рабочее дерево изменилось: %v", after)
	}

	// Вложенный репозиторий, появившийся после точки на новом месте, откат
	// оставляет как есть, а остальное откатывает.
	dir2 := gitRepo(t, true)
	g2, _ := OpenGit(dir2)
	snap2, _ := g2.CreateSnapshot(time.Now(), SnapshotMeta{})
	write(t, dir2, "cloned/lib.txt", "клон\n")
	git(t, filepath.Join(dir2, "cloned"), "init", "-q")
	git(t, filepath.Join(dir2, "cloned"), "add", "-A")
	git(t, filepath.Join(dir2, "cloned"), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "nested")
	write(t, dir2, "a.txt", "agent\n")
	plan, err := g2.PlanRollback(snap2)
	if err != nil || len(plan.Skipped) != 1 || plan.Skipped[0] != "cloned" {
		t.Fatalf("план: %+v %v", plan, err)
	}
	if _, _, err := g2.Rollback(snap2, time.Now()); err != nil {
		t.Fatal(err)
	}
	if read(t, dir2, "a.txt") != "original a\n" || read(t, dir2, "cloned/lib.txt") != "клон\n" {
		t.Fatal("a.txt должен откатиться, вложенный репозиторий — остаться")
	}
}

func TestWorktreesAreIsolated(t *testing.T) {
	main := gitRepo(t, true)
	other := filepath.Join(t.TempDir(), "second")
	git(t, main, "worktree", "add", "-q", other, "-b", "second")
	write(t, other, "a.txt", "second worktree\n")

	gMain, _ := OpenGit(main)
	gOther, _ := OpenGit(other)
	if gMain.worktreeID() != "main" || !strings.HasPrefix(gOther.worktreeID(), "wt-") {
		t.Fatalf("идентификаторы: %q %q", gMain.worktreeID(), gOther.worktreeID())
	}
	snapOther, err := gOther.CreateSnapshot(time.Now(), SnapshotMeta{})
	if err != nil {
		t.Fatal(err)
	}
	// Снимок второго дерева не виден из первого — ни в списке, ни по имени.
	if snaps, _ := gMain.Snapshots(); len(snaps) != 0 {
		t.Fatalf("в основном дереве виден чужой снимок: %+v", snaps)
	}
	if _, err := gMain.FindSnapshot(""); err == nil {
		t.Fatal("FindSnapshot выбрал снимок другого дерева")
	}
	if _, err := gMain.FindSnapshot(snapOther.Ref); err == nil {
		t.Fatal("снимок другого дерева найден по полной ссылке")
	}
	// Даже если снимок передан напрямую, откат отказывается.
	if _, _, err := gMain.Rollback(snapOther, time.Now()); err == nil || !strings.Contains(err.Error(), "другом рабочем дереве") {
		t.Fatalf("откат к чужому снимку: %v", err)
	}
	if got := read(t, main, "a.txt"); got != "original a\n" {
		t.Fatalf("основное дерево изменено: %q", got)
	}
	// Очистка одного дерева не трогает снимки другого.
	if _, err := gMain.CreateSnapshot(time.Now(), SnapshotMeta{}); err != nil {
		t.Fatal(err)
	}
	write(t, main, "a.txt", "main v2\n")
	if _, err := gMain.CreateSnapshot(time.Now(), SnapshotMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := gMain.Prune(1); err != nil {
		t.Fatal(err)
	}
	mainSnaps, _ := gMain.Snapshots()
	otherSnaps, _ := gOther.Snapshots()
	if len(mainSnaps) != 1 || len(otherSnaps) != 1 {
		t.Fatalf("после очистки: main=%d other=%d", len(mainSnaps), len(otherSnaps))
	}
}

func TestSnapshotOrder(t *testing.T) {
	dir := gitRepo(t, true)
	g, _ := OpenGit(dir)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) // все снимки «в один момент»
	var last Snapshot
	for i := 0; i < 15; i++ {
		write(t, dir, "a.txt", fmt.Sprintf("v%d\n", i))
		s, err := g.CreateSnapshot(now, SnapshotMeta{})
		if err != nil {
			t.Fatal(err)
		}
		last = s
		newest, err := g.FindSnapshot("")
		if err != nil || newest.Ref != last.Ref {
			t.Fatalf("шаг %d: самым свежим выбран %s, а создан %s", i, newest.Name(), last.Name())
		}
	}
	// Снимки старого формата (…-000, …-000-2, …-000-12) тоже по порядку.
	names := []string{"20261114-134625-000", "20261114-134625-000-2", "20261114-134625-000-9", "20261114-134625-000-12"}
	for i := 1; i < len(names); i++ {
		if nameKey(names[i-1]) >= nameKey(names[i]) {
			t.Errorf("порядок старых имён: %s должен быть раньше %s", names[i-1], names[i])
		}
	}
	if nameKey("20261114-134625-000-12") >= nameKey("20261114-134626-000000001") {
		t.Error("новый формат должен быть позже старого из предыдущей секунды")
	}
	// Старый снимок в refs/holdfast/<имя> виден в основном дереве.
	git(t, dir, "update-ref", "refs/holdfast/20200101-000000-000", last.Commit)
	snaps, _ := g.Snapshots()
	if got := snaps[len(snaps)-1].Name(); got != "20200101-000000-000" {
		t.Errorf("старый снимок должен быть последним в списке, а там %s", got)
	}
}

// realProject — настоящий git-репозиторий для проверки обратимости.
func realProject(t *testing.T) (*Policy, string) {
	t.Helper()
	dir := gitRepo(t, false)
	write(t, dir, ".gitignore", "*.log\nlocal/\nnode_modules/\n")
	for i := 0; i < 7; i++ {
		write(t, dir, fmt.Sprintf("assets/%d.png", i), "tracked")
	}
	git(t, dir, "add", "-A")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	for i := 0; i < 7; i++ {
		write(t, dir, fmt.Sprintf("drafts/%d.md", i), "untracked")
		write(t, dir, fmt.Sprintf("local/%d.bin", i), "ignored")
		write(t, dir, fmt.Sprintf("node_modules/pkg/%d.js", i), "ignored, regenerable")
	}
	p := NewPolicy(dir, dir, &config.GuardConfig{})
	p.Home = fakeHome
	p.Session = "s1"
	p.tempDirs = nil // фикстуры лежат во временной папке
	return p, dir
}

func TestMassDeleteOnlyWhenCovered(t *testing.T) {
	p, dir := realProject(t)
	outside := gitRepo(t, true)
	for i := 0; i < 7; i++ {
		write(t, outside, fmt.Sprintf("x%d.txt", i), "x")
	}
	tests := []struct {
		cmd  string
		want Decision
		rule string
	}{
		{"rm -r assets", Allow, ""},                      // отслеживаются git → попадут в точку
		{"rm -r drafts", Allow, ""},                      // новые, не игнорируются → попадут в точку
		{"rm drafts/*.md", Allow, ""},                    // то же через glob
		{"rm -rf node_modules", Allow, ""},               // пересоздаваемая папка
		{"rm -rf local", Deny, "mass-delete"},            // игнорируется → точка не вернёт
		{"rm -rf assets local", Deny, "mass-delete"},     // смешанный случай
		{"find assets -name '*.png' -delete", Allow, ""}, // под фильтр подходит только отслеживаемое
		{"find . -name '*.bin' -delete", Deny, "mass-delete"},
		{"find . -type f -delete", Deny, "delete-git-dir"}, // удалит и .git со всеми точками
		{"find . -name 'HEAD' -delete", Deny, "delete-git-dir"},
		{"rm -rf .git", Deny, "delete-git-dir"},
		{"find " + outside + " -type f -delete", Deny, "rm-outside-repo"},
		{"find .. -type f -delete", Deny, "rm-outside-repo"},
		{"find $DIR -delete", Deny, "rm-dynamic"},
		{"git clean -fd", Allow, ""},                            // 7 неотслеживаемых → в точке
		{"git -C " + outside + " clean -fd", Deny, "git-clean"}, // чужой репозиторий
		{"cd " + outside + " && git clean -fd", Deny, "git-clean"},
		{"git clean -fdx", Deny, "git-clean"},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got := p.CheckCommand(tt.cmd)
			if got.Decision != tt.want || got.Rule != tt.rule {
				t.Fatalf("%q = %s [%s: %s], want %s [%s]", tt.cmd, got.Decision, got.Rule, got.Reason, tt.want, tt.rule)
			}
		})
	}

	// Свойство: если массовое удаление разрешено, то каждый удаляемый файл
	// действительно лежит в самой свежей точке — её сделал сам хук.
	g, _ := OpenGit(dir)
	snaps, err := g.Snapshots()
	if err != nil || len(snaps) == 0 || snaps[0].Meta.Kind != KindPreDelete || snaps[0].Meta.Session != "s1" {
		t.Fatalf("точка перед удалением не создана: %+v %v", snaps, err)
	}
	saved, _ := g.TreeFiles(snaps[0].Tree, nil)
	for i := 0; i < 7; i++ {
		for _, f := range []string{fmt.Sprintf("assets/%d.png", i), fmt.Sprintf("drafts/%d.md", i)} {
			if !saved[f] {
				t.Errorf("%s нет в точке, хотя удаление было разрешено", f)
			}
		}
		if saved[fmt.Sprintf("local/%d.bin", i)] {
			t.Errorf("игнорируемый файл оказался в точке")
		}
	}
	// Подсказка при отказе объясняет причину и не предлагает обходов.
	v := p.CheckCommand("rm -rf local")
	if !strings.Contains(v.Reason, "local/0.bin") || !strings.Contains(v.Reason, "спроси пользователя") {
		t.Errorf("подсказка: %s", v.Reason)
	}
}

func TestFailedCheckpointIsVisibleAndGrantsNothing(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root игнорирует права доступа")
	}
	p, dir := realProject(t)
	env := testEnv(dir)
	in := `{"session_id":"s1","cwd":"` + dir + `","prompt":"первый запрос"}`
	var out bytes.Buffer
	RunCheckpoint(strings.NewReader(in), &out, env)
	if out.Len() != 0 || LastCheckpointError(dir) != "" {
		t.Fatalf("успешная точка должна быть молчаливой: %q %q", out.String(), LastCheckpointError(dir))
	}

	// Следующую точку записать нельзя.
	refs := filepath.Join(dir, ".git", "refs", "holdfast")
	if err := filepath.WalkDir(refs, func(path string, d fs.DirEntry, _ error) error {
		if d != nil && d.IsDir() {
			return os.Chmod(path, 0o500)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(refs, func(path string, d fs.DirEntry, _ error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	write(t, dir, "assets/0.png", "изменено после первой точки")
	RunCheckpoint(strings.NewReader(in), &out, env)

	var msg struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal(out.Bytes(), &msg); err != nil || !strings.Contains(msg.SystemMessage, "НЕ создана") {
		t.Fatalf("пользователь не предупреждён: %q (%v)", out.String(), err)
	}
	if LastCheckpointError(dir) == "" {
		t.Fatal("ошибка точки не сохранена для guard list")
	}
	entries, _ := ReadLog(dir)
	if last := entries[len(entries)-1]; last.Rule != "error" {
		t.Fatalf("в журнале нет ошибки: %+v", last)
	}
	// Старая точка не даёт права на массовое удаление: хук пробует сделать
	// новую, не может — и запрещает.
	if v := p.CheckCommand("rm -r assets"); v.Decision != Deny || !strings.Contains(v.Reason, "не удалось сохранить точку") {
		t.Fatalf("после неудачной точки: %s [%s]", v.Decision, v.Reason)
	}
}

func TestBrokenConfigDoesNotWeakenProtection(t *testing.T) {
	root := t.TempDir()
	write(t, root, "holdfast.toml", "[guard]\nprotected_paths = [\"migrations/**\"]\ndeny_commands = [\"terraform destroy\"]\n")
	target := map[string]any{"file_path": filepath.Join(root, "migrations", "001.sql"), "content": "x"}
	decide := func(tool string, input map[string]any) (string, string) {
		t.Helper()
		var out bytes.Buffer
		RunHook(bytes.NewReader(hookJSON(t, root, tool, input)), &out, testEnv(root))
		if out.Len() == 0 {
			return "allow", ""
		}
		var resp hookOutput
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.HookSpecificOutput.PermissionDecision, resp.HookSpecificOutput.PermissionDecisionReason
	}
	before := map[string]string{}
	cases := map[string]map[string]any{
		"write-protected": target,
		"deny-command":    {"command": "terraform destroy"},
		"plain-command":   {"command": "go test ./..."},
		"builtin-deny":    {"command": "rm -rf ~"},
	}
	tools := map[string]string{"write-protected": "Write", "deny-command": "Bash", "plain-command": "Bash", "builtin-deny": "Bash"}
	for name, input := range cases {
		before[name], _ = decide(tools[name], input)
	}
	if before["write-protected"] != "ask" || before["deny-command"] != "deny" || before["plain-command"] != "allow" {
		t.Fatalf("исходные решения: %v", before)
	}

	// Опечатка в конфиге (два варианта): защита не должна стать слабее ни в одном случае.
	rank := map[string]int{"allow": 0, "ask": 1, "deny": 2}
	for _, broken := range []string{
		"[guard]\nmode = \"typo\"\nprotected_paths = [\"migrations/**\"]\ndeny_commands = [\"terraform destroy\"]\n",
		"[guard\nprotected_paths = [\"migrations/**\"]\n",
	} {
		write(t, root, "holdfast.toml", broken)
		for name, input := range cases {
			got, reason := decide(tools[name], input)
			// deny_commands при нечитаемом конфиге недоступен — но действие
			// хотя бы требует подтверждения, а не проходит молча.
			floor := before[name]
			if floor == "deny" && name == "deny-command" {
				floor = "ask"
			}
			if rank[got] < rank[floor] || (got == "allow") {
				t.Errorf("%s: при битом конфиге %s (было %s) — защита ослаблена", name, got, before[name])
			}
			if got == "ask" && !strings.Contains(reason, "holdfast.toml") {
				t.Errorf("%s: причина не объясняет проблему с конфигом: %s", name, reason)
			}
		}
		if got, _ := decide("Read", map[string]any{"file_path": filepath.Join(root, "README.md")}); got != "allow" {
			t.Errorf("чтение при битом конфиге: %s", got)
		}
	}
	entries, _ := ReadLog(root)
	found := false
	for _, e := range entries {
		found = found || e.Rule == "config-error"
	}
	if !found {
		t.Error("в журнале нет события config-error")
	}
}

// Обёртки и эквивалентные формы команды получают то же решение.
func TestEquivalentFormsGetSameDecision(t *testing.T) {
	p := autoPolicy(t, fakeRepo(t, "ref: refs/heads/dev"))
	for _, cmd := range []string{
		"rm -rf ~",
		"env rm -rf ~",
		"env -u TEST rm -rf ~",
		"env -i FOO=1 rm -rf ~",
		"env --unset=TEST -C /tmp rm -rf ~",
		`env -S "rm -rf ~"`,
		"sudo env -u X rm -rf ~",
		"command rm -rf ~",
		"nohup env -u A -u B rm -rf ~",
	} {
		if got := p.CheckCommand(cmd); got.Decision != Deny || got.Rule != "rm-home" {
			t.Errorf("%q = %s [%s], want deny [rm-home]", cmd, got.Decision, got.Rule)
		}
	}
	if got := p.CheckCommand("env -u TEST go test ./..."); got.Decision != Allow {
		t.Errorf("безобидная команда через env: %s", got.Decision)
	}
}

func TestInstallEdgeCases(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".claude/settings.json", "null")
	if _, changed, err := InstallHook(root, false, ""); err != nil || !changed {
		t.Fatalf("settings.json = null: %v %v", changed, err)
	}
	// Путь с пробелом экранируется: иначе shell разобьёт его на два слова.
	other := t.TempDir()
	path, _, err := InstallHook(other, false, "/Users/me/My Tools/holdfast")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `'/Users/me/My Tools/holdfast' guard check`) {
		t.Fatalf("путь не экранирован: %s", data)
	}
	if got := shellQuoteArg("it's"); got != `'it'\''s'` {
		t.Errorf("кавычка: %s", got)
	}
	if _, changed, _ := InstallHook(other, false, "/Users/me/My Tools/holdfast"); changed {
		t.Error("повторная установка с тем же путём не должна ничего менять")
	}
}

// Дочерний shell — ( … ), $( … ), <( … ), bash -c, часть конвейера,
// фоновая команда, env -C — не меняет папку для следующих команд.
func TestChildShellDoesNotChangeParentContext(t *testing.T) {
	p := autoPolicy(t, fakeRepo(t, "ref: refs/heads/dev"))
	children := []string{
		"(cd %s)", "(cd %s; ls)", "bash -c 'cd %s'", `sh -c "cd %s && ls"`, "zsh -c 'cd %s'",
		"echo $(cd %s; pwd)", "cat <(cd %s; ls)", "cd %s &", "cd %s | cat", "ls | cd %s",
		"env -C %s ls", "sudo -D %s ls", "f() { cd %s; }", "pushd %s >/dev/null; popd",
	}
	follow := []string{"rm -rf projects", "rm -rf docs/old", "git push origin dev", "rm -rf build"}
	for _, dir := range []string{"/", "~", "/etc"} {
		for _, c := range children {
			child := fmt.Sprintf(c, dir)
			for _, f := range follow {
				want := p.CheckCommand(f)
				seps := []string{"; ", " && ", "\n"}
				switch {
				case strings.HasSuffix(child, "&"):
					seps = []string{"\n"} // «cmd &; …» — синтаксическая ошибка
				case strings.HasSuffix(child, "}"):
					// «f() { … } && x» парсер относит x к определению функции
					seps = []string{"; ", "\n"}
				}
				for _, sep := range seps {
					cmd := child + sep + f
					if got := p.CheckCommand(cmd); got.Decision != want.Decision || got.Rule != want.Rule {
						t.Errorf("%q = %s [%s], а без дочернего cd — %s [%s]", cmd, got.Decision, got.Rule, want.Decision, want.Rule)
					}
				}
			}
		}
	}
	// Контроль: cd в самом shell меняет папку, дочерний shell её наследует,
	// а вызов функции с cd делает папку неизвестной.
	for _, cmd := range []string{
		"cd / && rm -rf projects",
		"cd / && (rm -rf projects)",
		"cd / && bash -c 'rm -rf projects'",
		"pushd / && rm -rf projects",
		"f() { cd /; }; f; rm -rf projects",
		"env -C / rm -rf projects",
	} {
		if got := p.CheckCommand(cmd); got.Decision != Deny {
			t.Errorf("%q = %s [%s], want deny", cmd, got.Decision, got.Rule)
		}
	}
}

// pathProject — проект с секретом, запретом на go.sum, защищённой папкой и
// символическими ссылками на них.
func pathProject(t *testing.T) (*Policy, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		".env": "SECRET=1", "go.sum": "x", "notes.txt": "n", "migrations/001.sql": "x",
		"other/go.sum": "y", "other/readme.md": "r", "sub/keep.txt": "k",
	} {
		write(t, dir, name, content)
	}
	for link, target := range map[string]string{"alias.txt": ".env", "lock.link": "go.sum", "dangling.txt": ".env.local"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPolicy(dir, dir, &config.GuardConfig{
		ProtectedPaths: []string{"migrations/**"},
		Synced: config.SyncedConfig{Rules: []config.SyncedRule{
			{Kind: config.KindDenyPath, Pattern: "go.sum", Source: "CLAUDE.md:1"},
		}},
	})
	p.Home = fakeHome
	p.tempDirs = nil
	return p, dir
}

// Запреты путей и секретов применяются к итоговому файлу: glob, {…},
// символическая ссылка и cp в папку дают то же решение, что прямой путь.
func TestEquivalentPathFormsGetSameDecision(t *testing.T) {
	p, _ := pathProject(t)
	groups := []struct {
		want  Decision
		rule  string
		forms []string
	}{
		{Deny, "secret-file", []string{
			"cat .env", "cat .en[v]", "cat .e?v", "cat .e{n,x}v", "cat ./sub/../.env", "cat alias.txt",
			"head -n1 .en*", "cat < .en[v]", "grep SECRET alia?.txt", "cat dangling.txt", "cat *.txt",
		}},
		{Deny, "synced-deny-path", []string{
			"echo x > go.sum", "echo x > go.s[u]m", "echo x > go.{sum,mod}", "rm go.su*", "echo x > lock.link",
			"cp other/go.sum .", "cp other/go.sum ./", "cp other/go.sum sub/..", "cp -t . other/go.sum",
			"cp --target-directory=. other/go.sum", "mv notes.txt go.sum", "cp -r other/. .", "rsync -a other/ .",
			"sed -i s/a/b/ go.su?", "tee lock.link", "ln -sf notes.txt go.sum", "dd if=/dev/zero of=go.sum",
		}},
		{Ask, "protected-path", []string{
			"touch migrations/002.sql", "cp notes.txt migrations/", "cp notes.txt migr*/", "rm -r migrations",
			"echo x > migrations/00[1].sql", "rm -r migr*",
		}},
		{Allow, "", []string{
			"cat notes.txt", "cat 'note?.txt'", "ls -la", "cp other/readme.md .", "rm other/go.sum",
			"echo x > sub/new.txt", "cat sub/*.txt", "cp notes.txt sub/",
		}},
	}
	for _, g := range groups {
		for _, cmd := range g.forms {
			if got := p.CheckCommand(cmd); got.Decision != g.want || got.Rule != g.rule {
				t.Errorf("%q = %s [%s], want %s [%s]", cmd, got.Decision, got.Rule, g.want, g.rule)
			}
		}
	}
	// Инструменты Read/Edit/Write тоже идут по ссылке.
	if got := p.CheckFile(OpRead, "alias.txt"); got.Rule != "secret-file" {
		t.Errorf("Read alias.txt = %s [%s], want secret-file", got.Decision, got.Rule)
	}
	if got := p.CheckFile(OpWrite, "lock.link"); got.Rule != "synced-deny-path" {
		t.Errorf("Edit lock.link = %s [%s], want synced-deny-path", got.Decision, got.Rule)
	}
}

// Служебные файлы Holdfast — настройки защиты и точки восстановления —
// агент не меняет ни правкой, ни командой; читать их можно.
func TestServiceFilesAreProtected(t *testing.T) {
	p, dir := realProject(t)
	write(t, dir, "holdfast.toml", "[guard]\n")
	write(t, dir, ".claude/settings.json", "{}")
	write(t, dir, ".holdfast/checkpoints/s1", "x")
	if err := os.Symlink("holdfast.toml", filepath.Join(dir, "cfg.link")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "update-ref", "refs/holdfast/main/x", "HEAD")

	files := []string{
		"holdfast.toml", ".claude/settings.json", ".claude/settings.local.json", ".holdfast/checkpoints/s1",
		".git/refs/holdfast/main/x", ".git/packed-refs", "~/.claude/settings.json", "cfg.link", "holdfast.tom?",
	}
	forms := []string{
		"echo x > %s", "echo x >> %s", "sed -i s/a/b/ %s", "cp drafts/0.md %s", "mv %s /tmp/x", "rm %s",
		"tee %s", "truncate -s0 %s", "rm -f %s", "chmod 000 %s", "ln -sf /dev/null %s", "(echo x > %s)",
		"bash -c 'echo x > %s'",
	}
	for _, f := range files {
		for _, form := range forms {
			cmd := fmt.Sprintf(form, f)
			if got := p.CheckCommand(cmd); got.Decision != Deny {
				t.Errorf("%q = %s [%s], want deny", cmd, got.Decision, got.Rule)
			}
		}
		if strings.ContainsAny(f, "?") {
			continue
		}
		if got := p.CheckFile(OpWrite, f); got.Decision != Deny || got.Rule != "holdfast-config" {
			t.Errorf("Edit %s = %s [%s], want deny [holdfast-config]", f, got.Decision, got.Rule)
		}
	}
	for cmd, want := range map[string]Decision{
		"rm -rf .claude":                              Deny,
		"mv .claude /tmp/c":                           Deny,
		"rm -rf .holdfast":                            Deny,
		"chmod -R 000 .holdfast":                      Deny,
		"cp -r /tmp/x .holdfast/":                     Deny,
		"holdfast sync":                               Ask,
		"holdfast sync --yes":                         Ask,
		"holdfast -C . init":                          Ask,
		"holdfast guard install":                      Ask,
		"holdfast guard uninstall --local":            Ask,
		"holdfast -C . guard uninstall --purge --yes": Ask,
		"holdfast sync --dry-run":                     Allow,
		"cat holdfast.toml":                           Allow,
		"mkdir -p .claude/agents":                     Allow,
		"echo x > .claude/notes":                      Allow,
	} {
		if got := p.CheckCommand(cmd); got.Decision != want {
			t.Errorf("%q = %s [%s], want %s", cmd, got.Decision, got.Rule, want)
		}
	}
	if got := p.CheckFile(OpRead, "holdfast.toml"); got.Decision != Allow {
		t.Errorf("Read holdfast.toml = %s, want allow", got.Decision)
	}
	// strict: правку решает пользователь.
	strict := NewPolicy(dir, dir, &config.GuardConfig{Mode: config.ModeStrict})
	strict.Home = fakeHome
	if got := strict.CheckFile(OpWrite, "holdfast.toml"); got.Decision != Ask || got.Rule != "holdfast-config" {
		t.Errorf("strict Edit holdfast.toml = %s [%s], want ask", got.Decision, got.Rule)
	}
}

// pushRepo — «репозиторий» на ветке dev с ветками main и dev и, по
// желанию, конфигурацией .git/config.
func pushRepo(t *testing.T, gitConfig string, extra map[string]string) *Policy {
	t.Helper()
	root := fakeRepo(t, "ref: refs/heads/dev")
	write(t, root, ".git/refs/heads/main", "1111111111111111111111111111111111111111\n")
	write(t, root, ".git/refs/heads/dev", "2222222222222222222222222222222222222222\n")
	if gitConfig != "" {
		write(t, root, ".git/config", gitConfig)
	}
	for name, content := range extra {
		write(t, root, name, content)
	}
	return autoPolicy(t, root)
}

// Push в защищённую ветку запрещён, как бы ни было задано назначение;
// если назначение не определить — отдельный запрет с подсказкой.
func TestPushDestinationIsResolved(t *testing.T) {
	p := pushRepo(t, "", nil)
	cases := []struct {
		want  string // правило; "" — разрешено
		forms []string
	}{
		{"git-push-protected", []string{
			"git push origin main", "git push origin HEAD:main", "git push origin dev:refs/heads/main",
			"git -c remote.origin.push=HEAD:main push", "git -c Remote.origin.PUSH=HEAD:main push",
			"git -c remote.origin.push=refs/heads/dev:refs/heads/main push origin",
			"git -c remote.origin.push=refs/heads/dev:refs/heads/main push origin dev",
			"git -c push.default=upstream -c branch.dev.merge=refs/heads/main push",
			"git switch main && git push", "git checkout main; git push origin HEAD", "git branch -m main && git push",
			"(git switch main); git push", "git -C . switch main && git push",
		}},
		{"git-push-unclear", []string{
			"git -c push.default=matching push", "git push origin $BRANCH", "git push $REMOTE",
			"git -c remote.origin.push=$X push", "git --config-env=remote.origin.push=V push",
			"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=remote.origin.push GIT_CONFIG_VALUE_0=HEAD:main git push",
			"env GIT_DIR=/x git push origin dev", "export GIT_CONFIG_GLOBAL=/tmp/x; git push",
			"git config remote.origin.push HEAD:main && git push", "echo '[push]' >> .git/config && git push",
			"git checkout $B && git push", "git checkout v1.0 && git push", "git checkout - && git push",
			"git push origin 'refs/heads/*:refs/heads/*'", "git push origin :",
			"git switch -c x origin/main && git push",
		}},
		{"git-push-force", []string{"git -c remote.origin.mirror=true push", "git -c remote.origin.push=+HEAD:dev push"}},
		{"git-push-tags", []string{"git -c push.followTags=true push origin dev"}},
		{"", []string{
			"git push origin dev", "git push", "git push origin HEAD", "git -c push.default=current push",
			"git switch -c feature && git push -u origin feature", "git -c core.autocrlf=false push origin dev",
			"git switch feature && git push", "GIT_SSH_COMMAND='ssh -i k' git push origin dev",
			"git -c push.followTags=true push --no-follow-tags origin dev", "git config --get push.default && git push",
			"git checkout notes.md && git push",
		}},
	}
	write(t, p.Root, "notes.md", "x")
	for _, c := range cases {
		for _, cmd := range c.forms {
			got := p.CheckCommand(cmd)
			if c.want == "" && got.Decision != Allow || c.want != "" && (got.Decision != Deny || got.Rule != c.want) {
				t.Errorf("%q = %s [%s], want %q", cmd, got.Decision, got.Rule, c.want)
			}
		}
	}

	// То же назначение из файлов конфигурации git, включая include.
	for _, tc := range []struct {
		name, config string
		extra        map[string]string
		want         string
	}{
		{"remote.origin.push", "[remote \"origin\"]\n\tpush = HEAD:main\n", nil, "git-push-protected"},
		{"upstream", "[push]\n\tdefault = upstream ; комментарий\n[branch \"dev\"]\n\tmerge = refs/heads/main\n", nil, "git-push-protected"},
		{"include", "[include]\n\tpath = extra.cfg\n", map[string]string{".git/extra.cfg": "[push]\ndefault = matching\n"}, "git-push-unclear"},
		{"mirror", "[remote \"origin\"]\n\tmirror\n", nil, "git-push-force"},
		{"simple", "[push]\n\tdefault = simple\n[branch \"dev\"]\n\tmerge = refs/heads/main\n", nil, ""},
	} {
		p := pushRepo(t, tc.config, tc.extra)
		got := p.CheckCommand("git push")
		if tc.want == "" && got.Decision != Allow || tc.want != "" && got.Rule != tc.want {
			t.Errorf("%s: git push = %s [%s], want %q", tc.name, got.Decision, got.Rule, tc.want)
		}
	}
}
