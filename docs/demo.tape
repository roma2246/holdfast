# Сценарий демо-гифки для README. Запись: ./docs/demo/record.sh
# (VHS в Docker, https://github.com/charmbracelet/vhs)
#
# История: проект работает → просим агента → запуск сломан → смотрим точки
# восстановления → откатываем → снова работает. Русские подписи выводят
# функции step и ask из docs/demo/setup.sh (VHS путает кириллицу при наборе).

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

Type "holdfast guard install"
Enter
Sleep 3s
Type "./start.sh"
Enter
Sleep 2.5s

Hide
Type "clear"
Enter
Type "step 2"
Enter
Type "ask"
Enter
Show
Sleep 2.5s
Type `agent run "git push origin main"`
Enter
Sleep 4s
Type `agent run "rm -r assets"`
Enter
Sleep 2s
Type `agent run "mv lib/cart.sh lib/basket.sh"`
Enter
Sleep 2s

Hide
Type "step 3"
Enter
Show
Type "./start.sh"
Enter
Sleep 3.5s

Hide
Type "clear"
Enter
Type "step 4"
Enter
Show
Type "holdfast guard list"
Enter
Sleep 4.5s
Type "holdfast guard rollback --to 1"
Enter
Sleep 3.5s
Type "y"
Enter
Sleep 2.5s
Type "./start.sh"
Enter
Sleep 3.5s

Hide
Type "clear"
Enter
Type "step 5"
Enter
Show
Type "holdfast lint"
Enter
Sleep 7s
