#!/usr/bin/env bash
# Runs the official MCP conformance suite against its reference server, once
# directly and once through Vigie's gateway, for each protocol revision the
# suite defines requirements for. Every scenario that passes directly must
# pass through Vigie.
#
# Usage: test/conformance/run.sh   (make conformance; needs bin/vigie, Node 22, git)
set -euo pipefail

# Pinned: the suite and its reference server, as published together.
CONFORMANCE_VERSION=0.2.0-alpha.12
CONFORMANCE_COMMIT=c37eec888e1c6ff140af79987a40008548b7cc5f
REVISIONS=(2025-11-25 2026-07-28)
SERVER_PORT=${SERVER_PORT:-3311}
VIGIE_PORT=${VIGIE_PORT:-8799}

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
vigie=${VIGIE_BIN:-$root/bin/vigie}
work=${CONFORMANCE_WORK:-$root/bin/conformance}
[ -x "$vigie" ] || { echo "$vigie not found: run make build first" >&2; exit 1; }
mkdir -p "$work"
pids=()
cleanup() { for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done; }
trap cleanup EXIT

wait_for() {
  for _ in $(seq 1 100); do
    curl -s -o /dev/null "$1" && return 0
    sleep 0.2
  done
  echo "$1 did not come up" >&2
  return 1
}

echo "== Reference server ($CONFORMANCE_COMMIT)"
if [ ! -d "$work/repo/.git" ]; then
  git init -q "$work/repo"
  git -C "$work/repo" remote add origin https://github.com/modelcontextprotocol/conformance
fi
git -C "$work/repo" fetch -q --depth 1 origin "$CONFORMANCE_COMMIT"
git -C "$work/repo" checkout -q --force FETCH_HEAD
server_dir="$work/repo/examples/servers/typescript"
(cd "$server_dir" && npm ci --no-audit --no-fund --silent)
(cd "$server_dir" && PORT=$SERVER_PORT exec npx tsx everything-server.ts > "$work/server.log" 2>&1) &
pids+=($!)

echo "== Conformance suite $CONFORMANCE_VERSION"
npm install --prefix "$work/cli" --no-audit --no-fund --silent "@modelcontextprotocol/conformance@$CONFORMANCE_VERSION"
conformance="$work/cli/node_modules/.bin/conformance"

# Pinning trusts every new tool: the suite checks that the protocol goes
# through untouched, and adds tools on the fly.
rm -rf "$work/pins"
cat > "$work/vigie.yaml" <<YAML
version: 1
pins_dir: $work/pins
servers:
  everything:
    url: http://127.0.0.1:$SERVER_PORT/mcp
    pins:
      on_new: pin
YAML
"$vigie" gateway --config "$work/vigie.yaml" --listen "127.0.0.1:$VIGIE_PORT" > "$work/vigie.log" 2>&1 &
pids+=($!)
wait_for "http://127.0.0.1:$SERVER_PORT/mcp"
wait_for "http://127.0.0.1:$VIGIE_PORT/healthz"

status=0
cd "$work"
for revision in "${REVISIONS[@]}"; do
  echo "== Requirements of revision $revision"
  "$conformance" server --url "http://127.0.0.1:$SERVER_PORT/mcp" --requirements "$revision" > "direct-$revision.txt" 2>&1 || true
  "$conformance" server --url "http://127.0.0.1:$VIGIE_PORT/mcp/everything" --requirements "$revision" > "vigie-$revision.txt" 2>&1 || true
  echo "   directly:      $(grep -E '^Total' "direct-$revision.txt")"
  echo "   through Vigie: $(grep -E '^Total' "vigie-$revision.txt")"
  python3 "$here/compare.py" "direct-$revision.txt" "vigie-$revision.txt" | sed 's/^/   /' || status=1
done
if grep -qE 'level=(WARN|ERROR)' vigie.log; then
  echo "== Vigie logged warnings:"
  grep -E 'level=(WARN|ERROR)' vigie.log | sed 's/^time=[^ ]* //' | sort | uniq -c | sed 's/^/   /'
fi
exit $status
