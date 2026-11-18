#!/usr/bin/env bash
# make bench-setup: installs what `make bench` needs besides Go: k6 (HTTP load generator, from
# Grafana's apt repository) and python3-matplotlib (charts). Asks for sudo. Safe to re-run.
set -euo pipefail

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

if [ "$(id -u)" -eq 0 ]; then SUDO=""; else SUDO="sudo"; fi

say "python3-matplotlib"
$SUDO apt-get update -qq
$SUDO apt-get install -y -qq python3-matplotlib gpg ca-certificates curl

say "k6"
if command -v k6 >/dev/null 2>&1; then
  echo "k6 already installed"
else
  KEYRING=/usr/share/keyrings/k6-archive-keyring.gpg
  if [ ! -s "$KEYRING" ]; then
    curl -fsSL https://dl.k6.io/key.gpg | $SUDO gpg --dearmor -o "$KEYRING"
  fi
  echo "deb [signed-by=$KEYRING] https://dl.k6.io/deb stable main" | $SUDO tee /etc/apt/sources.list.d/k6.list >/dev/null
  $SUDO apt-get update -qq
  $SUDO apt-get install -y -qq k6
fi

say "versions"
k6 version
"${BEEVM_PYTHON:-/usr/bin/python3}" -c "import matplotlib; print('matplotlib', matplotlib.__version__)"
