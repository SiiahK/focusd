#!/usr/bin/env bash
#
# build.sh — cross-compile do daemon focusd, 100% CGO-free (Pilar 6).
# ---------------------------------------------------------------------------
# Graças ao driver SQLite PURO em Go (modernc.org/sqlite), compilamos para
# Linux e macOS (amd64/arm64) a partir de QUALQUER máquina, sem toolchain C,
# sem osxcross, sem zig. CGO_ENABLED=0 também gera binário ESTÁTICO — roda em
# qualquer libc (Alpine incluso), o que torna o installer `curl | bash` sólido.
#
# Uso:  ./build.sh
# Saída: release_v1/focusd-<os>-<arch>  +  SHA256SUMS.txt
# ---------------------------------------------------------------------------
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$ROOT/release_v1"
BIN=focusd

# Targets de release. Adicionar um SO/arch é só acrescentar aqui.
TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
)

# Scripts que acompanham o binário na release (fonte no root do repo).
SHIP_SCRIPTS=(focus_status.sh focus_tracker.lua focusd.sh)

# sha256: sha256sum (Linux) ou shasum -a 256 (macOS/BSD).
if command -v sha256sum >/dev/null 2>&1; then
  SHA() { sha256sum "$@"; }
  SHA_VERIFY="sha256sum -c SHA256SUMS.txt"
elif command -v shasum >/dev/null 2>&1; then
  SHA() { shasum -a 256 "$@"; }
  SHA_VERIFY="shasum -a 256 -c SHA256SUMS.txt"
else
  echo "erro: nem sha256sum nem shasum encontrados" >&2; exit 1
fi

mkdir -p "$OUT"
cd "$ROOT"

echo "▶ compilando (CGO_ENABLED=0) …"
for t in "${TARGETS[@]}"; do
  os="${t%/*}"; arch="${t#*/}"
  out="$OUT/${BIN}-${os}-${arch}"
  # -trimpath: paths reproduzíveis; -ldflags "-s -w": sem tabela de símbolos
  # nem DWARF → binário menor. GOFLAGS herdado do ambiente é respeitado.
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w" -o "$out" .
  size=$(du -h "$out" | cut -f1)
  echo "  ✓ ${BIN}-${os}-${arch}  (${size})"
done

# Scripts de release: copia as versões atuais do root para a pasta de release.
echo "▶ empacotando scripts …"
for s in "${SHIP_SCRIPTS[@]}"; do
  if [ -f "$ROOT/$s" ]; then
    cp "$ROOT/$s" "$OUT/$s"
    echo "  ✓ $s"
  else
    echo "  ⚠ $s não encontrado no root — mantendo o que já está em release_v1/" >&2
  fi
done

# Checksums: um único SHA256SUMS.txt sobre binários + scripts, ordenado.
# Rodar de DENTRO de OUT deixa os nomes relativos (sem o caminho absoluto),
# que é o formato que `sha256sum -c` espera.
echo "▶ gerando SHA256SUMS.txt …"
cd "$OUT"
: > SHA256SUMS.txt
for f in $(printf '%s\n' "${BIN}"-*-* "${SHIP_SCRIPTS[@]}" | sort); do
  [ -f "$f" ] && SHA "$f" >> SHA256SUMS.txt
done
echo "  ✓ $(wc -l < SHA256SUMS.txt | tr -d ' ') artefatos assinados"

echo
echo "✅ Release pronta em release_v1/"
echo "   Verificar integridade:  (cd release_v1 && ${SHA_VERIFY})"
