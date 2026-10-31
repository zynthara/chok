#!/usr/bin/env bash
# smoke.sh — examples/blog as a real process (`make smoke`): build the
# binary, start it on a random port, wait for /healthz, find that request
# in /metrics on the metrics listener's own random port, check the App's
# port does not serve /metrics, send SIGINT and expect a clean exit
# within 3 seconds. The
# drain delay is set to zero: it is a pause for load balancers, which a
# smoke test has none of.
# Portable to the bash 3.2 that macOS ships.
set -euo pipefail

cd "$(dirname "$0")/.."
work=$(mktemp -d)
pid=""
cleanup() {
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid" 2>/dev/null || true; fi
	rm -rf "$work"
}
trap cleanup EXIT
fail() {
	echo "smoke: $*" >&2
	echo "--- blog log" >&2
	cat "$work/log" >&2 || true
	exit 1
}

go build -o "$work/blog" ./examples/blog

# The binary reads examples/blog/chok.yaml; the environment overrides it.
# The signing key is a fixed development key: a smoke test is not
# production, and a real deployment generates its own.
(cd examples/blog && exec env BLOG_HTTP_ADDR=127.0.0.1:0 BLOG_TELEMETRY_METRICS_ADDR=127.0.0.1:0 BLOG_HTTP_DRAIN_DELAY=0s \
	BLOG_DB_SQLITE_PATH="$work/blog.db" BLOG_AUTH_SIGNING_KEY=smoke-test-signing-key-not-a-secret-32b \
	"$work/blog" >"$work/log" 2>&1) &
pid=$!

# The App logs the address it bound: {"msg":"chok: http listening","addr":"127.0.0.1:NNNNN"}.
addr=""
for _ in $(seq 1 100); do
	addr=$(sed -n 's/.*"msg":"chok: http listening","addr":"\([^"]*\)".*/\1/p' "$work/log" | head -1)
	[ -n "$addr" ] && break
	kill -0 "$pid" 2>/dev/null || fail "exited before listening"
	sleep 0.1
done
[ -n "$addr" ] || fail "no listening address after 10s"

code=""
for _ in $(seq 1 50); do
	code=$(curl -s -o "$work/health" -w '%{http_code}' "http://$addr/healthz" || true)
	[ "$code" = 200 ] && break
	sleep 0.1
done
[ "$code" = 200 ] || fail "GET /healthz = $code"

# telemetry counted that request, under the probe's own route, and
# serves it on its own listener, which it logs before the App listens:
# {"msg":"telemetry: metrics listening","addr":"127.0.0.1:NNNNN",...}.
maddr=$(sed -n 's/.*"msg":"telemetry: metrics listening","addr":"\([^"]*\)".*/\1/p' "$work/log" | head -1)
[ -n "$maddr" ] || fail "no metrics listening address"
code=$(curl -s -o "$work/metrics" -w '%{http_code}' "http://$maddr/metrics" || true)
[ "$code" = 200 ] || fail "GET $maddr/metrics = $code"
grep -q '^http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/healthz"} ' "$work/metrics" ||
	fail "/metrics does not count the /healthz request"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://$addr/metrics" || true)
[ "$code" = 404 ] || fail "the App's port serves /metrics: $code"

kill -INT "$pid"
start=$(date +%s)
for _ in $(seq 1 30); do
	kill -0 "$pid" 2>/dev/null || break
	sleep 0.1
done
if kill -0 "$pid" 2>/dev/null; then fail "still running 3s after SIGINT"; fi
status=0
wait "$pid" || status=$?
pid=""
[ "$status" -eq 0 ] || fail "exit status $status after SIGINT"
grep -q '"msg":"chok: stopped"' "$work/log" || fail "no stop log line"
echo "smoke: examples/blog listened on $addr, /healthz 200 $(cat "$work/health"), /metrics 200 on $maddr and 404 on $addr, stopped on SIGINT in $(($(date +%s) - start))s with status 0"
