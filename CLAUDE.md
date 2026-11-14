# Holdfast

CLI-утилита на Go: `holdfast lint` проверяет файлы инструкций агентов,
`holdfast guard` — PreToolUse-хук Claude Code, снапшоты и откат,
`holdfast sync` — переносит запреты из CLAUDE.md в политику guard.

## Commands

- Сборка: `go build ./...`
- Запуск: `go run ./cmd/holdfast --help`
- Тесты: `go test ./...`
- Покрытие: `go test -coverpkg=./internal/... -coverprofile=cover.out ./... && go tool cover -func=cover.out | tail -1`
- Бенчмарк хука: `go test -bench=BenchmarkHook -run='^$' ./internal/guard/`
- Линтер: `golangci-lint run ./...`
- Формат: `gofmt -w .`
- Локальная сборка релиза: `goreleaser build --snapshot --clean`

## Structure

- `cmd/holdfast/main.go` — точка входа, только вызывает `cli.Execute`.
- `internal/cli/` — команды cobra; коды выхода: 0 ок, 1 находки lint, 2 сбой.
- `internal/config/` — `holdfast.toml`; секцию `[guard.synced]` пишет только `sync`.
- `internal/lint/` — парсер, реестр правил, отчёт; `internal/lint/rules/` — одно правило = один файл.
- `internal/guard/` — политика (`policy.go`, `shell.go`, `massdelete.go`), хуки (`hook.go`, `checkpoint.go`), журнал, снимки и откат (`snapshot.go`), установка хуков.
- `internal/bridge/` — извлечение запретов из CLAUDE.md.
- `testdata/lint/HFxxx/{bad,good}/` — фикстуры правил, `testdata/lint/all/` — все правила сразу.

## Conventions

- Новое правило lint: файл `internal/lint/rules/hfNNN_name.go` с `lint.Register` в `init()`,
  фикстуры `testdata/lint/HFNNN/bad` и `good`, ID в списке в `rules_test.go`.
- Команды shell разбираются только через AST (`mvdan.cc/sh/v3/syntax`), без поиска подстрок.
- Новый случай guard — строка в табличном тесте: `TestAutonomousMode` (режим по умолчанию) или `TestCheckCommand` (strict).
- В autonomous-режиме запрет всегда объясняет агенту, что сделать вместо; «спросить» — только по явной настройке пользователя.
- Хук не должен падать и должен отвечать быстрее 10 мс: проверяйте `TestHookLatency`.
- Комментарии к неочевидным конструкциям Go пишем по-русски.
- Покрытие не ниже 80%.
- Гарантии (откат без потерь, изоляция worktree, обратимость удаления) проверяются тестами-свойствами в `guarantees_test.go`; новая гарантия — новое свойство там.
- Обещания в README не сильнее того, что проверено тестами: чего хук не ловит, записано в разделе «Ограничения».

<!-- holdfast
deny-paths: go.sum
ask-commands: goreleaser release, git tag
-->
