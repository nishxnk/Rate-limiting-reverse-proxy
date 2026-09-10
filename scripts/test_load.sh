#!/usr/bin/env bash
#
# Load and threshold check for the rate-limiting reverse proxy.
#
# Fires REQUESTS concurrent requests at the proxy, tallies the status codes,
# and checks that the number of 2xx responses matches the configured policy.
#
#   ./scripts/test_load.sh
#   REQUESTS=500 CONCURRENCY=50 ./scripts/test_load.sh
#   API_KEY=tenant-a TARGET=http://localhost:8080/anything ./scripts/test_load.sh
#
set -euo pipefail

TARGET=${TARGET:-http://localhost:8080/}
ADMIN_PREFIX=${ADMIN_PREFIX:-/_rl}
BASE=${BASE:-$(printf '%s' "$TARGET" | sed -E 's#(https?://[^/]+).*#\1#')}
REQUESTS=${REQUESTS:-200}
CONCURRENCY=${CONCURRENCY:-25}
API_KEY=${API_KEY:-}

bold=$(printf '\033[1m'); dim=$(printf '\033[2m'); reset=$(printf '\033[0m')
green=$(printf '\033[32m'); red=$(printf '\033[31m'); yellow=$(printf '\033[33m')

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "${red}error:${reset} $1 is required but not installed" >&2; exit 1; }
}
need curl
need awk

echo "${bold}rate limiter load test${reset}"
echo "  target       ${TARGET}"
echo "  requests     ${REQUESTS}"
echo "  concurrency  ${CONCURRENCY}"
echo "  identity     ${API_KEY:-<client ip>}"
echo

# --- reachability ---------------------------------------------------------
if ! curl -sS -o /dev/null --max-time 5 "${BASE}${ADMIN_PREFIX}/healthz"; then
  echo "${red}error:${reset} ${BASE} is not reachable. Start the proxy first:" >&2
  echo "         docker compose up --build   (or)   make run" >&2
  exit 1
fi

# --- current policy -------------------------------------------------------
POLICY_JSON=$(curl -sS --max-time 5 "${BASE}${ADMIN_PREFIX}/api/policy" 2>/dev/null || echo '')
LIMIT=$(printf '%s' "$POLICY_JSON" | sed -n 's/.*"rps":\([0-9]*\).*/\1/p')
WINDOW=$(printf '%s' "$POLICY_JSON" | sed -n 's/.*"window_seconds":\([0-9.]*\).*/\1/p')
LIMIT=${LIMIT:-unknown}
WINDOW=${WINDOW:-unknown}
echo "${dim}policy: ${LIMIT} requests per ${WINDOW}s${reset}"

# --- header probe ---------------------------------------------------------
echo
echo "${bold}rate limit headers${reset}"
if [ -n "$API_KEY" ]; then
  curl -sS -D - -o /dev/null -H "X-API-Key: ${API_KEY}" "$TARGET"
else
  curl -sS -D - -o /dev/null "$TARGET"
fi | awk 'tolower($0) ~ /^(x-ratelimit|retry-after|http\/)/ {printf "  %s\n", $0}'

# --- clear previous state so the threshold check is meaningful -------------
curl -sS -o /dev/null -X POST "${BASE}${ADMIN_PREFIX}/api/clients/reset" 2>/dev/null || true
sleep "$(awk -v w="${WINDOW}" 'BEGIN{ if (w+0 > 0) print (w+0.2); else print 1.2 }')"

# --- the burst ------------------------------------------------------------
RESULTS=$(mktemp)
trap 'rm -f "$RESULTS"' EXIT

fire() {
  if [ -n "${API_KEY:-}" ]; then
    curl -sS -o /dev/null -w '%{http_code} %{time_total}\n' \
         -H "X-API-Key: ${API_KEY}" --max-time 10 "${TARGET}" || echo "000 0"
  else
    curl -sS -o /dev/null -w '%{http_code} %{time_total}\n' \
         --max-time 10 "${TARGET}" || echo "000 0"
  fi
}
export -f fire
export TARGET API_KEY

echo
echo "${bold}firing ${REQUESTS} requests at concurrency ${CONCURRENCY}${reset}"
START=$(date +%s.%N 2>/dev/null || date +%s)
seq 1 "$REQUESTS" | xargs -P "$CONCURRENCY" -I{} bash -c 'fire' > "$RESULTS"
END=$(date +%s.%N 2>/dev/null || date +%s)
ELAPSED=$(awk -v a="$START" -v b="$END" 'BEGIN{d=b-a; if (d<=0) d=0.001; printf "%.3f", d}')

# --- results --------------------------------------------------------------
echo
echo "${bold}status distribution${reset}"
awk '{ count[$1]++ } END { for (code in count) print code, count[code] }' "$RESULTS" | sort -n | awk -v total="$REQUESTS" '
      {
        code = $1; n = $2
        pct = (n * 100) / total
        bars = int(pct / 2)
        bar = ""
        for (i = 0; i < bars; i++) bar = bar "#"
        label = (code == "200" ? "OK" : (code == "429" ? "THROTTLED" : (code == "000" ? "FAILED" : "")))
        print sprintf("  %-4s %6d  %5.1f%%  %-50s %s", code, n, pct, bar, label)
      }'

OK=$(awk '$1 == 200 {n++} END {print n+0}' "$RESULTS")
BLOCKED=$(awk '$1 == 429 {n++} END {print n+0}' "$RESULTS")
FAILED=$(awk '$1 !~ /^(200|429)$/ {n++} END {print n+0}' "$RESULTS")

echo
echo "${bold}latency${reset}"
sort -k2 -n "$RESULTS" | awk '
  function pct(q,   i) { i = int(q * NR + 0.5); if (i < 1) i = 1; if (i > NR) i = NR; return lat[i] }
  { lat[NR] = $2 * 1000; sum += lat[NR] }
  END {
    if (NR == 0) { print "  no samples"; exit }
    print sprintf("  mean %7.2f ms", sum / NR)
    print sprintf("  p50  %7.2f ms", pct(0.50))
    print sprintf("  p95  %7.2f ms", pct(0.95))
    print sprintf("  p99  %7.2f ms", pct(0.99))
    print sprintf("  max  %7.2f ms", lat[NR])
  }'

echo
echo "${bold}summary${reset}"
printf "  %-22s %s s\n" "wall clock" "$ELAPSED"
printf "  %-22s %s\n" "throughput" "$(awk -v r="$REQUESTS" -v e="$ELAPSED" 'BEGIN{printf "%.0f req/s", r/e}')"
printf "  %-22s %s\n" "allowed (200)" "$OK"
printf "  %-22s %s\n" "throttled (429)" "$BLOCKED"
printf "  %-22s %s\n" "failed" "$FAILED"
printf "  %-22s %s\n" "policy limit" "$LIMIT per ${WINDOW}s"

# --- verdict --------------------------------------------------------------
echo
if [ "$FAILED" -gt 0 ]; then
  echo "${red}FAIL${reset}  ${FAILED} requests did not complete"
  exit 1
fi
if [ "$LIMIT" = "unknown" ]; then
  echo "${yellow}NOTE${reset}  could not read the policy from ${BASE}${ADMIN_PREFIX}/api/policy, skipping the threshold check"
  exit 0
fi

# The burst usually spans more than one window, so a little over the limit is
# expected and correct. Anything far above it means the limiter leaked.
CEILING=$(awk -v l="$LIMIT" -v e="$ELAPSED" -v w="$WINDOW" 'BEGIN{ printf "%d", l * (1 + (e / (w > 0 ? w : 1))) + 1 }')
if [ "$OK" -le "$CEILING" ] && [ "$BLOCKED" -gt 0 ]; then
  echo "${green}PASS${reset}  ${OK} allowed and ${BLOCKED} throttled, within the ${LIMIT}/${WINDOW}s policy (ceiling ${CEILING})"
else
  echo "${red}FAIL${reset}  ${OK} requests were allowed, expected at most ${CEILING} under a ${LIMIT}/${WINDOW}s policy"
  exit 1
fi
