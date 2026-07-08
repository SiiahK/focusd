#!/usr/bin/env bash
# tmux_focus.sh — registra uma sessão de foco na API focusd (Bash puro)
# Uso: ./tmux_focus.sh {habit_id} {duration_minutes}
# Ex.: ./tmux_focus.sh 1 25
#
# Pensado para bind no tmux, por exemplo:
#   bind-key F run-shell "~/focusd/tmux_focus.sh 1 25"

set -euo pipefail

# Resolve o Unix socket do mesmo jeito que o daemon (runtimeDir do daemon.go).
if [ -n "${FOCUSD_SOCK:-}" ]; then
  SOCK="$FOCUSD_SOCK"
elif [ -n "${XDG_RUNTIME_DIR:-}" ]; then
  SOCK="$XDG_RUNTIME_DIR/focusd/focusd.sock"
elif [ "$(uname)" = "Darwin" ]; then
  SOCK="$HOME/Library/Caches/focusd/focusd.sock"
else
  SOCK="${XDG_CACHE_HOME:-$HOME/.cache}/focusd/focusd.sock"
fi

# notify exibe uma mensagem de status. Dentro do tmux ($TMUX definido) usa
# display-message; fora dele faz apenas um echo no terminal padrão, para que o
# script rode sem erros quando chamado à mão, fora de uma sessão tmux.
# ${TMUX:-} evita que o `set -u` aborte quando a variável não existe.
notify() {
  if [ -n "${TMUX:-}" ] && command -v tmux >/dev/null 2>&1; then
    tmux display-message "$1"
  else
    echo "$1"
  fi
}

if [ "$#" -ne 2 ]; then
  echo "uso: $0 {habit_id} {duration_minutes}" >&2
  exit 1
fi

habit_id="$1"
duration_minutes="$2"

# valida que ambos são inteiros positivos.
case "$habit_id$duration_minutes" in
  *[!0-9]*)
    echo "erro: habit_id e duration_minutes devem ser inteiros" >&2
    exit 1
    ;;
esac

# Mesmo contrato de 250ms dos demais clientes: um keybind do tmux jamais
# pode pendurar esperando o daemon. O host da URL é ignorado com --unix-socket.
response=$(curl -s --max-time 0.25 --unix-socket "$SOCK" -X POST \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "habit_id=${habit_id}" \
  --data-urlencode "duration_minutes=${duration_minutes}" \
  "http://localhost/focus" 2>/dev/null) || { notify "focusd fora do ar"; exit 0; }

notify "${response:-foco de ${duration_minutes}m registrado}"
