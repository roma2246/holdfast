# Сценарий демо-гифки для README. Запись: vhs docs/demo.tape
# Установка VHS: brew install vhs (https://github.com/charmbracelet/vhs)

Output docs/demo.gif
Set FontSize 16
Set Width 1200
Set Height 720
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

Hide
Type "cd $(mktemp -d) && git init -q && cp -r $OLDPWD/testdata/lint/all/. . && clear"
Enter
Show

Type "# 1. Проверяем CLAUDE.md"
Enter
Type "holdfast lint"
Enter
Sleep 3s

Type "clear && # 2. Хук блокирует опасную команду"
Enter
Type `echo '{"tool_name":"Bash","tool_input":{"command":"ls && rm -rf ~"}}' | holdfast guard check | jq .`
Enter
Sleep 3s

Type "clear && # 3. Снапшот и откат"
Enter
Type "holdfast guard start && echo 'oops' > CLAUDE.md && holdfast guard diff --stat"
Enter
Sleep 2s
Type "holdfast guard rollback --yes && head -3 CLAUDE.md"
Enter
Sleep 3s

Type "holdfast guard log"
Enter
Sleep 4s
