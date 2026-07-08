#!/usr/bin/env bash
# scripts/demo_seed.sh — prepara o ambiente ISOLADO dos demos e grava os
# exports em /tmp/focusd_demo.env (que as fitas carregam em um instante —
# nada pesado roda DURANTE a gravação do VHS).
#
# Uso:  ./scripts/demo_seed.sh && vhs demo.tape       (hook + report)
#       ./scripts/demo_seed.sh && vhs demo_tui.tape   (dashboard interativo)
#
# Nada aqui toca o focusd real do usuário: socket, lock (XDG_RUNTIME_DIR),
# banco e binário vivem todos num mktemp descartável. Os dados semeados são
# SINTÉTICOS e plausíveis — é um demo; o estimador do report é o de produção.
# Limpeza depois de gravar:  pkill -f /tmp/fdemo && rm -rf /tmp/fdemo.*

set -euo pipefail
cd "$(dirname "$0")/.."

DEMO=$(mktemp -d /tmp/fdemo.XXXX)
export FOCUSD_SOCK="$DEMO/s.sock" FOCUSD_DB="$DEMO/focus.db"
export XDG_RUNTIME_DIR="$DEMO/run" XDG_STATE_HOME="$DEMO/state"

go build -o "$DEMO/bin/focusd" .
export PATH="$DEMO/bin:$PATH"

focusd &
sleep 0.5

# 12 dias de atividade (streak 12 no hook) · janela de 7 dias no report:
# myapp: ~45min/dia de go + ~12min/dia de lua · dotfiles: commits esparsos.
python3 - <<'EOF' > "$DEMO/seed.json"
import json, time
now = int(time.time())
hbs = []
# minutos de go por dia, variados de propósito: barras iguais não contam
# história nenhuma no gráfico do dashboard
go_by_day = [52, 34, 61, 18, 73, 41, 27, 55, 38, 66, 22, 48]
for d in range(12):
    day = now - d * 86400
    for m in range(go_by_day[d]):
        hbs.append({"project": "myapp", "file": "api.go", "language": "go",
                    "events": 7, "at": day - 3600 - m * 60})
    for m in range(6 + (d * 5) % 14):
        hbs.append({"project": "myapp", "file": "conf.lua", "language": "lua",
                    "events": 3, "at": day - 14400 - m * 60})
    if d % 3 == 0:
        hbs.append({"project": "dotfiles", "file": "", "language": "git",
                    "events": 1, "at": day - 7200})
print(json.dumps({"source": "nvim", "heartbeats": hbs}))
EOF
curl -s --unix-socket "$FOCUSD_SOCK" -H 'Content-Type: application/json' \
  -d @"$DEMO/seed.json" http://localhost/heartbeat > /dev/null
curl -s --unix-socket "$FOCUSD_SOCK" -d 'name=Deep Work' http://localhost/habits > /dev/null
curl -s --unix-socket "$FOCUSD_SOCK" -d 'name=Writing' http://localhost/habits > /dev/null
curl -s --unix-socket "$FOCUSD_SOCK" -d 'habit_id=1&duration_minutes=52' http://localhost/focus > /dev/null
curl -s --unix-socket "$FOCUSD_SOCK" -d 'habit_id=1&duration_minutes=45' http://localhost/focus > /dev/null

# repositório de exemplo onde o hook será instalado durante a gravação
mkdir -p "$DEMO/myapp"
cd "$DEMO/myapp"
git init -q
git config user.email demo@focusd && git config user.name "focusd demo"
printf 'package api\n' > api.go
git add api.go

# env que o demo.tape carrega no primeiro frame (invisível)
cat > /tmp/focusd_demo.env <<ENV
export DEMO="$DEMO"
export FOCUSD_SOCK="$FOCUSD_SOCK" FOCUSD_DB="$FOCUSD_DB"
export XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR" XDG_STATE_HOME="$XDG_STATE_HOME"
export PATH="$DEMO/bin:\$PATH"
ENV

echo "✓ demo pronto: $DEMO — agora rode: vhs demo.tape"
