#!/usr/bin/env bash
# Runs BeeVM's isolation tests, which need root and KVM (jailer, cgroups, network namespaces).
# Do not call this directly: `make test-isolation` first compiles the tests as your user (so the
# Go build cache stays yours) into bin/test/<package>.test, then runs this script with sudo.
#   RUN=<regexp>  only tests whose name matches (go test -run)
set -u
[ "$(id -u)" -eq 0 ] || { echo "needs root; run: make test-isolation"; exit 1; }
cd "$(dirname "$0")/.." || exit 1

rc=0
for t in bin/test/*.test; do
  [ -e "$t" ] || { echo "no test binaries in bin/test (run: make test-isolation)"; exit 1; }
  pkg=$(basename "$t" .test)
  echo "== internal/$pkg (as root)"
  # Run inside the package folder, like go test does (tests use relative paths).
  run="${RUN:-}"
  # The api package also has the normal end-to-end tests: as root, run only the isolation ones.
  if [ -z "$run" ]; then
    case "$pkg" in api) run='TestIsolation' ;; *) run='.' ;; esac
  fi
  (cd "internal/$pkg" && "../../$t" -test.v -test.count=1 -test.timeout 30m -test.run "$run") || rc=1
done

# Give files written as root back to the user who ran sudo.
if [ -n "${SUDO_UID:-}" ] && [ -d bench/results ]; then
  chown -R "$SUDO_UID:$SUDO_GID" bench/results 2>/dev/null
fi
exit $rc
