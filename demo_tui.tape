# demo_tui.tape — grava docs/demo_tui.gif com VHS (charmbracelet/vhs).
#
# Roteiro do dashboard interativo: abre o `focusd tui`, vai à aba sessions,
# inicia uma sessão de foco com enter, volta ao dashboard para vê-la AO VIVO
# junto do streak e do gráfico diário, e alterna a janela do gráfico.
#
#   ./scripts/demo_seed.sh && vhs demo_tui.tape
#
# Mesmo ambiente isolado do demo.tape (ver scripts/demo_seed.sh).

Output docs/demo_tui.gif

Set Shell "bash"
Set FontSize 16
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

Type "focusd tui"
Sleep 500ms
Enter
Sleep 3s

# aba 3: sessions — navega até "Writing" e inicia o foco
Type "3"
Sleep 1500ms
Type "j"
Sleep 800ms
Enter
Sleep 2500ms

# aba 1: dashboard — a sessão pulsa ao vivo sobre o streak e o gráfico
Type "1"
Sleep 3500ms

# d, d: cicla a janela do gráfico (14 → 30 → 7 dias)
Type "d"
Sleep 1s
Type "d"
Sleep 2500ms

# aba 2: projects — o report interativo
Type "2"
Sleep 3s

Hide
Type "q"
Sleep 500ms
Type `pkill -f "$DEMO/bin/focusd"`
Enter
