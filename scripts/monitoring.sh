#!/usr/bin/env bash
# Optional module (M10): Prometheus + Grafana for beevmd, from pinned release tarballs (no Docker).
#   bash scripts/monitoring.sh install   download and unpack if missing
#   bash scripts/monitoring.sh run       install if needed, run both in the foreground (Ctrl-C stops)
# Prometheus: http://127.0.0.1:9090 (scrapes beevmd at 127.0.0.1:8080/metrics every second)
# Grafana:    http://localhost:3000   (dashboard "BeeVM"; anonymous viewing, admin/admin to edit)
# Deleting this script, deploy/monitoring/ and the Makefile's monitoring targets removes the
# module; nothing else depends on it.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "$here/common.sh"

conf="$(cd "$here/../deploy/monitoring" && pwd)"
mon="$BEEVM_HOME/monitoring"
prom_dir="$mon/prometheus-$PROMETHEUS_VERSION"
graf_dir="$mon/grafana-$GRAFANA_VERSION"

# unpack TARBALL DEST: extract into DEST, dropping the tarball's top folder (its name varies).
unpack() {
  local tarball="$1" dest="$2"
  rm -rf "$dest.part"
  mkdir -p "$dest.part"
  tar -xzf "$tarball" -C "$dest.part" --strip-components=1
  mv "$dest.part" "$dest"
}

install_monitoring() {
  mkdir -p "$mon" "$CACHE_DIR"
  if [ -x "$prom_dir/prometheus" ]; then
    skip "prometheus $PROMETHEUS_VERSION"
  else
    local tb="$CACHE_DIR/prometheus-$PROMETHEUS_VERSION.linux-amd64.tar.gz"
    download "https://github.com/prometheus/prometheus/releases/download/v$PROMETHEUS_VERSION/prometheus-$PROMETHEUS_VERSION.linux-amd64.tar.gz" \
      "$tb" "$PROMETHEUS_SHA256"
    unpack "$tb" "$prom_dir"
  fi
  if [ -x "$graf_dir/bin/grafana" ]; then
    skip "grafana $GRAFANA_VERSION"
  else
    local tb="$CACHE_DIR/grafana-$GRAFANA_VERSION.linux-amd64.tar.gz"
    download "https://dl.grafana.com/oss/release/grafana-$GRAFANA_VERSION.linux-amd64.tar.gz" "$tb" "$GRAFANA_SHA256"
    unpack "$tb" "$graf_dir"
  fi
}

run_monitoring() {
  install_monitoring
  mkdir -p "$mon/prometheus-data" "$mon/grafana-data" "$mon/logs"

  # Grafana reads the dashboard folder from this variable in the provisioning file.
  export BEEVM_DASHBOARDS="$conf/grafana/dashboards"
  export GF_PATHS_DATA="$mon/grafana-data"
  export GF_PATHS_LOGS="$mon/logs"
  export GF_PATHS_PLUGINS="$mon/grafana-data/plugins"
  export GF_PATHS_PROVISIONING="$conf/grafana/provisioning"
  export GF_SERVER_HTTP_ADDR="127.0.0.1"
  export GF_SERVER_HTTP_PORT="3000"
  export GF_AUTH_ANONYMOUS_ENABLED="true"          # local demo only: anyone on this machine may view
  export GF_AUTH_ANONYMOUS_ORG_ROLE="Viewer"
  export GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH="$conf/grafana/dashboards/beevm.json"
  export GF_ANALYTICS_REPORTING_ENABLED="false"
  export GF_ANALYTICS_CHECK_FOR_UPDATES="false"
  export GF_NEWS_NEWS_FEED_ENABLED="false"

  "$prom_dir/prometheus" --config.file="$conf/prometheus.yml" \
    --storage.tsdb.path="$mon/prometheus-data" --storage.tsdb.retention.time=2d \
    --web.listen-address=127.0.0.1:9090 >"$mon/logs/prometheus.log" 2>&1 &
  local prom_pid=$!
  "$graf_dir/bin/grafana" server --homepath "$graf_dir" >"$mon/logs/grafana.log" 2>&1 &
  local graf_pid=$!
  # Ctrl-C stops both; if one of them dies, the other is stopped too.
  stop_both() { kill "$prom_pid" "$graf_pid" 2>/dev/null; wait 2>/dev/null; }
  trap 'stop_both; log "monitoring stopped"; exit 0' INT TERM

  log "Prometheus http://127.0.0.1:9090  (log: $mon/logs/prometheus.log)"
  log "Grafana    http://localhost:3000   dashboard BeeVM (log: $mon/logs/grafana.log)"
  log "start beevmd serve in another terminal; Ctrl-C here stops both"
  wait -n "$prom_pid" "$graf_pid" || true
  stop_both
  die "Prometheus or Grafana exited; see the logs in $mon/logs"
}

case "${1:-run}" in
  install) install_monitoring ;;
  run) run_monitoring ;;
  *) die "usage: monitoring.sh install|run" ;;
esac
