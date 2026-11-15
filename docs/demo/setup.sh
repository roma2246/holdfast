#!/usr/bin/env bash
# Prepares a demo project in a temp directory and the helper commands for
# the script. Sourced from docs/demo.tape.
demo=$(mktemp -d)/shop
mkdir -p "$demo/lib" "$demo/assets" "$demo/migrations"
cd "$demo" || return

cat >CLAUDE.md <<'EOF'
# Shop API

The API code lives in `src/server/`.

## Commands

- Run: `./start.sh`
- Deploy: `make deploy`

## Rules

- Never edit `migrations/`: they are already applied in production.
- Deploy token: ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789
EOF
cat >start.sh <<'EOF'
#!/usr/bin/env bash
source lib/config.sh && source lib/cart.sh &&
	echo "✓ shop is running on port $PORT" || { echo "✗ shop failed to start"; exit 1; }
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

# The captions and the user's prompt are not typed in the tape: these
# functions print them and also erase the line with their own command.

# step N prints the caption of a scene.
step() {
	local msg
	case $1 in
	1) msg="1. Connect Holdfast once. The project works" ;;
	2) msg="2. Ask the agent. Holdfast saves a restore point on its own" ;;
	3) msg="3. The agent is done, and the app no longer starts" ;;
	4) msg="4. See what changed and put everything back" ;;
	5) msg="5. Also: a check of the agent's instructions" ;;
	esac
	printf '\e[1A\e[2K\e[1;35m# %s\e[0m\n' "$msg"
}

# ask is the user's prompt to the agent. It sends the UserPromptSubmit hook the
# same JSON as Claude Code does, so the restore point is created for real.
ask() {
	local prompt="Rename cart to basket and remove what we don't need"
	jq -nc --arg cwd "$PWD" --arg p "$prompt" \
		'{session_id: "demo", cwd: $cwd, hook_event_name: "UserPromptSubmit", prompt: $p}' |
		CLAUDE_PROJECT_DIR=$PWD holdfast guard checkpoint
	printf '\e[1A\e[2K\e[1myou → agent:\e[0m "%s"\n' "$prompt"
	printf '\e[2mholdfast: restore point saved\e[0m\n\n'
}
