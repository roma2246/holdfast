#!/usr/bin/env bash
# Готовит демо-проект во временной папке: CLAUDE.md с типичными проблемами,
# миграции и git-репозиторий. Вызывается из docs/demo.tape через `source`.
demo=$(mktemp -d)/shop
mkdir -p "$demo/migrations" "$demo/src"
cd "$demo" || return

cat >CLAUDE.md <<'EOF'
# Shop API

Код API лежит в `src/server/`, документация — в docs/overview.md.

## Rules

- Use yarn for dependencies.
- Never use yarn in CI scripts.
- Write good code.
- Never edit `migrations/` — они уже применены в продакшене.
- Deploy token: ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789

Run end-to-end tests with `npm run e2e`.
EOF
echo '{"scripts": {"test": "jest"}}' >package.json
echo 'CREATE TABLE users (id INT);' >migrations/001_init.sql
echo 'console.log("shop")' >src/index.js

git init -q
git checkout -q -b feature/cart
git add -A
git -c user.name=demo -c user.email=demo@example.com commit -qm init
export PS1='\[\e[1;34m\]shop\[\e[0m\] $ '

# step N печатает подпись к сцене. VHS неправильно «набирает» кириллицу,
# поэтому русский текст не печатается вручную, а выводится этой функцией,
# которая заодно стирает свою строку с командой.
step() {
	local msg
	case $1 in
	1) msg="1. Проверяем инструкции агента: локально, без LLM" ;;
	2) msg="2. Агент ошибается: хук не просто запрещает, а подсказывает, как правильно" ;;
	3) msg="3. Правило проекта приходит агенту в момент, когда он открывает файл" ;;
	4) msg="4. Снапшот перед сессией и откат всего, что сделал агент" ;;
	5) msg="5. Журнал: что агенту разрешили, что запретили и почему" ;;
	esac
	printf '\e[1A\e[2K\e[1;35m# %s\e[0m\n' "$msg"
}
