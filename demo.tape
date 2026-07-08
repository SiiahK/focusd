# demo.tape — grava docs/demo.gif com VHS (charmbracelet/vhs).
#
# Roteiro em 3 atos: instala o hook → um commit acende o streak → o report
# mostra a semana. Rode da raiz do repo:
#
#   ./scripts/demo_seed.sh && vhs demo.tape
#
# O seed roda ANTES da gravação (nada pesado em cena) e deixa o ambiente
# 100% isolado em /tmp: binário recém-compilado, socket/lock/banco próprios,
# dados de amostra — nunca toca o focusd real do usuário.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 17
Set Width 1000
Set Height 620
Set Padding 14
Set Theme "Catppuccin Mocha"
Set TypingSpeed 55ms

Hide
Type `source /tmp/focusd_demo.env && cd "$DEMO/myapp" && clear`
Enter
Sleep 1s
Show

Type "focusd init --hook"
Sleep 500ms
Enter
Sleep 2s

Type `git commit -m "feat: ship the thing"`
Sleep 500ms
Enter
Sleep 3s

Type "focusd report"
Sleep 500ms
Enter
Sleep 5s

Hide
Type `pkill -f "$DEMO/bin/focusd"`
Enter
