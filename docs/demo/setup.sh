#!/usr/bin/env bash
# Готовит демо-проект во временной папке и вспомогательные команды для
# сценария. Вызывается из docs/demo.tape через `source`.
demo=$(mktemp -d)/shop
mkdir -p "$demo/lib" "$demo/assets" "$demo/migrations"
cd "$demo" || return

cat >CLAUDE.md <<'EOF'
# Shop API

Код API лежит в `src/server/`.

## Commands

- Запуск: `./start.sh`
- Деплой: `make deploy`

## Rules

- Never edit `migrations/` — они уже применены в продакшене.
- Deploy token: ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789
EOF
cat >start.sh <<'EOF'
#!/usr/bin/env bash
source lib/config.sh && source lib/cart.sh &&
	echo "✓ shop запущен на порту $PORT" || { echo "✗ shop не запустился"; exit 1; }
EOF
chmod +x start.sh
echo 'PORT=8080' >lib/config.sh
echo 'cart_total() { echo 0; }' >lib/cart.sh
printf 'test:\n\t./start.sh\n' >Makefile
echo 'CREATE TABLE users (id INT);' >migrations/001_init.sql
for i in 1 2 3 4 5 6; do echo "icon $i" >"assets/icon$i.svg"; done

git init -q
git checkout -q -b feature/cart
git add -A
git -c user.name=demo -c user.email=demo@example.com commit -qm init
export PS1='\[\e[1;34m\]shop\[\e[0m\] $ '

# VHS неправильно «набирает» кириллицу, поэтому русский текст в сценарии не
# печатается вручную: подписи и запрос пользователя выводят эти функции,
# которые заодно стирают строку со своей командой.

# step N — подпись к сцене.
step() {
	local msg
	case $1 in
	1) msg="1. Подключаем Holdfast один раз. Проект работает" ;;
	2) msg="2. Просим агента. Holdfast сам сохраняет точку восстановления" ;;
	3) msg="3. Агент закончил, а запуск сломан" ;;
	4) msg="4. Смотрим, что изменилось, и возвращаем как было" ;;
	5) msg="5. А ещё: проверка инструкций агента" ;;
	esac
	printf '\e[1A\e[2K\e[1;35m# %s\e[0m\n' "$msg"
}

# ask — запрос пользователя к агенту. Отправляет хуку UserPromptSubmit тот же
# JSON, что и Claude Code, поэтому точка восстановления создаётся по-настоящему.
ask() {
	local prompt="Переименуй cart в basket и убери лишнее"
	jq -nc --arg cwd "$PWD" --arg p "$prompt" \
		'{session_id: "demo", cwd: $cwd, hook_event_name: "UserPromptSubmit", prompt: $p}' |
		CLAUDE_PROJECT_DIR=$PWD holdfast guard checkpoint
	printf '\e[1A\e[2K\e[1mвы → агенту:\e[0m «%s»\n' "$prompt"
	printf '\e[2mholdfast: точка восстановления сохранена\e[0m\n\n'
}
