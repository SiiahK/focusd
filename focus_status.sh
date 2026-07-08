#!/usr/bin/env bash
#
# focus_status.sh — componente de status-line do tmux para o focusd.
# ---------------------------------------------------------------------------
# Faz um curl RÁPIDO e SILENCIOSO em GET /status e imprime o cronômetro do foco
# ativo. Filosofia "onipresença sem atrito": quando NÃO há foco (ou o servidor
# está fora), imprime NADA — a barra fica limpa, zero ruído cognitivo.
#
# O dot pulsa (● / ○) a cada segundo — combine com `status-interval 1` no tmux.
#
# Config:
#   FOCUSD_SOCK  caminho do Unix socket do daemon
#                (default: $XDG_RUNTIME_DIR/focusd/focusd.sock, com fallback
#                 pra ~/Library/Caches no macOS e ~/.cache no Linux)
# ---------------------------------------------------------------------------

# Resolve o socket do mesmo jeito que o daemon (runtimeDir do daemon.go).
if [ -n "${FOCUSD_SOCK:-}" ]; then
  SOCK="$FOCUSD_SOCK"
elif [ -n "${XDG_RUNTIME_DIR:-}" ]; then
  SOCK="$XDG_RUNTIME_DIR/focusd/focusd.sock"
elif [ "$(uname)" = "Darwin" ]; then
  SOCK="$HOME/Library/Caches/focusd/focusd.sock"
else
  SOCK="${XDG_CACHE_HOME:-$HOME/.cache}/focusd/focusd.sock"
fi

# Contrato de timeout implacável: 250ms de teto. A status-line JAMAIS trava
# esperando o daemon. -f trata 4xx/5xx como falha; qualquer erro → exit 0
# silencioso (barra limpa). O host na URL é ignorado com --unix-socket.
resp="$(curl -s -f --max-time 0.25 --unix-socket "$SOCK" \
        http://localhost/status 2>/dev/null)" || exit 0

case "$resp" in
  *Foco:*)
    mins="${resp##*Foco: }"                       # "🎯 Foco: 15m" -> "15m"
    if (( $(date +%s) % 2 )); then dot="●"; else dot="○"; fi   # pulso 1Hz
    # #[...] são interpretados pelo tmux; hex exige tmux >= 2.9 (senão use 'green').
    printf '#[fg=#a6e3a1]%s 🎯 %s#[default]' "$dot" "$mins"
    ;;
  *)
    exit 0                                         # ocioso/offline -> barra limpa
    ;;
esac
