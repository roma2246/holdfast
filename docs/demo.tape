# Сценарий демо-гифки для README. Запись: ./docs/demo/record.sh
# (VHS в Docker, https://github.com/charmbracelet/vhs)

Output docs/demo.gif

Set Shell bash
Set FontSize 15
Set Width 1500
Set Height 860
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms
Set PlaybackSpeed 1.0

Hide
Type "source /usr/local/share/holdfast-demo/setup.sh && clear"
Enter
Type "step 1"
Enter
Show

Type "holdfast lint"
Enter
Sleep 6s

Hide
Type "clear"
Enter
Type "step 2"
Enter
Show
Type `agent bash "ls && rm -rf ~"`
Enter
Sleep 2.5s
Type `agent bash "git push origin main"`
Enter
Sleep 3.5s
Type `agent bash "git push -u origin feature/cart"`
Enter
Sleep 2s

Hide
Type "step 3"
Enter
Show
Type "agent read migrations/001_init.sql"
Enter
Sleep 4.5s

Hide
Type "clear"
Enter
Type "step 4"
Enter
Show
Type "holdfast guard start"
Enter
Sleep 1s
Type "echo 'broken' > CLAUDE.md && rm package.json && holdfast guard diff --stat"
Enter
Sleep 3s
Type "holdfast guard rollback --yes && git status --short && head -1 CLAUDE.md"
Enter
Sleep 4s

Hide
Type "clear"
Enter
Type "step 5"
Enter
Show
Type "holdfast guard log -n 4"
Enter
Sleep 6s
