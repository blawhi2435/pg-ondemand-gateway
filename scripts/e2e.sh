#!/usr/bin/env bash
# e2e.sh — runs design §10.2's 12 end-to-end scenarios against the
# environment scripts/setup.sh installed, printing PASS/FAIL per scenario.
# Assumes: setup.sh has already run, the e2e-client pod is Ready, and
# certs/{testca,rogueca}.crt exist locally (from scripts/gen-certs.sh).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAMESPACE=pgproxy-e2e
CLIENT_POD=e2e-client

pass=0
fail=0
results=()

record() {
	local n="$1" status="$2" detail="$3"
	results+=("$n|$status|$detail")
	if [[ "$status" == PASS ]]; then
		pass=$((pass + 1))
	else
		fail=$((fail + 1))
	fi
	echo "[$status] scenario $n: $detail"
}

client_exec() { kubectl exec -n "$NAMESPACE" "$CLIENT_POD" -- sh -c "$1"; }

pw() { kubectl get secret "$1" -n "$NAMESPACE" -o jsonpath='{.data.password}' | base64 -d; }

PW1="$(pw tenant1-db-app)"
PW2="$(pw tenant2-db-app)"

# sum_metric_prefix totals every counter/gauge sample value across every
# pg-proxy pod whose line starts with the given (fixed-string) prefix —
# each pod's Registry/Router state is independent, so a global picture
# needs a per-pod scrape summed together.
sum_metric_prefix() {
	local prefix="$1" total=0
	for p in $(kubectl get pods -n "$NAMESPACE" -l app=pg-proxy -o jsonpath='{.items[*].metadata.name}'); do
		kubectl port-forward -n "$NAMESPACE" "$p" 19999:9090 >/tmp/pgproxy-e2e-pf-metrics.log 2>&1 &
		local pf=$!
		sleep 1.5
		local v
		v="$(curl -s http://127.0.0.1:19999/metrics | grep -F "$prefix" | awk '{s+=$2} END{print s+0}')"
		kill "$pf" 2>/dev/null
		wait "$pf" 2>/dev/null
		total=$((total + ${v%.*}))
	done
	echo "$total"
}
sum_active() { sum_metric_prefix "pgproxy_connections_active"; }
sum_unknown_sni() { sum_metric_prefix 'pgproxy_handshake_errors_total{reason="unknown_sni"}'; }

# --- Scenario 1 & 2: verify-full to two tenants lands on different backends ---
# Assert a positive match (looks like an IPv4 address) rather than just
# "doesn't contain the word ERROR" — psql's connection-level failures
# (e.g. "server closed the connection unexpectedly") don't contain that
# word either and would otherwise slip through as a false PASS.
ipv4_re='^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'
out1="$(client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant1.db.test port=5432 dbname=app user=app_rw sslmode=verify-full' -tAc 'select inet_server_addr()'" 2>&1)"
out2="$(client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW2'; psql 'host=tenant2.db.test port=5432 dbname=app user=app_rw sslmode=verify-full' -tAc 'select inet_server_addr()'" 2>&1)"
if [[ "$out1" =~ $ipv4_re ]]; then
	record 1 PASS "verify-full to tenant1.db.test succeeded (backend $out1)"
else
	record 1 FAIL "tenant1.db.test connection failed: $out1"
fi
if [[ "$out2" =~ $ipv4_re ]] && [[ "$out2" != "$out1" ]]; then
	record 2 PASS "tenant2.db.test landed on a different backend ($out2 != $out1)"
else
	record 2 FAIL "tenant2.db.test did not land on a distinct backend: $out2 (tenant1 was $out1)"
fi

# --- Scenario 3: rogue CA must fail ---
out3="$(client_exec "export PGSSLROOTCERT=/etc/e2e-ca/rogueca.crt; psql 'host=tenant1.db.test port=5432 dbname=app user=app_rw sslmode=verify-full connect_timeout=5' -c 'select 1'" 2>&1)"
if [[ "$out3" == *"certificate verify failed"* ]] || [[ "$out3" == *"SSL error"* ]]; then
	record 3 PASS "connection with an unrelated root CA was rejected (verify-full is real, not a disguised require)"
else
	record 3 FAIL "expected a certificate verification failure, got: $out3"
fi

# --- Scenario 4: unknown SNI ---
apisix_ip="$(kubectl get svc apisix-gateway -n "$NAMESPACE" -o jsonpath='{.spec.clusterIP}')"
out4="$(client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt; psql 'host=unknown.db.test hostaddr=$apisix_ip port=5432 dbname=app user=app_rw sslmode=require connect_timeout=5' -c 'select 1'" 2>&1)"
if [[ "$out4" == *"unknown host"* ]]; then
	record 4 PASS "unknown SNI rejected cleanly with an explicit ErrorResponse, no hang"
else
	record 4 FAIL "expected an 'unknown host' ErrorResponse, got: $out4"
fi

# --- Scenario 5: exceed maxConnsPerCluster (per-pod — target one replica directly) ---
# Design's limits are per-pod (§8.1: "maxConns ⚠ per-pod，3 replica = 全域
# 15000"), so this must target one specific replica directly rather than
# going through APISIX's round-robin, which would spread 11 connections
# across 3 pods and never trip any single pod's limit of 10.
one_pod="$(kubectl get pods -n "$NAMESPACE" -l app=pg-proxy -o jsonpath='{.items[0].metadata.name}')"
one_pod_ip="$(kubectl get pod "$one_pod" -n "$NAMESPACE" -o jsonpath='{.status.podIP}')"
tmp_dir="$(mktemp -d)"
for i in $(seq 1 11); do
	client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant1.db.test hostaddr=$one_pod_ip port=5432 dbname=app user=app_rw sslmode=verify-full' -c 'select pg_sleep(3)'" >"$tmp_dir/o$i.log" 2>&1 &
done
wait
if grep -l "too many connections" "$tmp_dir"/o*.log >/dev/null 2>&1; then
	record 5 PASS "the 11th concurrent connection to one pod (limit 10) got 53300/too many connections"
else
	record 5 FAIL "expected at least one 'too many connections' rejection among 11 concurrent connections to a single pod"
fi
rm -rf "$tmp_dir"

# --- Scenario 6: cert hot reload; existing connection unaffected ---
client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant1.db.test port=5432 dbname=app user=app_rw sslmode=verify-full' -c 'select pg_sleep(20)'" >/tmp/pgproxy-e2e-scenario6.log 2>&1 &
scenario6_pid=$!
sleep 2
kubectl create secret tls pg-proxy-tls -n "$NAMESPACE" \
	--cert="$ROOT/certs/wildcard-v2.crt" --key="$ROOT/certs/wildcard-v2.key" \
	--dry-run=client -o yaml | kubectl apply -f - >/dev/null
sleep 35 # > tls.reloadInterval (30s in this environment's ConfigMap)
new_conn_out="$(client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant1.db.test port=5432 dbname=app user=app_rw sslmode=verify-full' -tAc 'select 1'" 2>&1)"
wait "$scenario6_pid" 2>/dev/null
old_conn_ok=0
grep -q "pg_sleep" /tmp/pgproxy-e2e-scenario6.log 2>/dev/null && old_conn_ok=1
if [[ "$new_conn_out" == "1" ]] && [[ "$old_conn_ok" == 1 ]]; then
	record 6 PASS "cert rotated (new connections still succeed) and the pre-rotation connection was not disrupted"
else
	record 6 FAIL "new_conn_out=$new_conn_out old_conn_ok=$old_conn_ok"
fi

# --- Scenario 7 & 8: add/remove a Pooler ---
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: postgresql.cnpg.io/v1
kind: Pooler
metadata:
  name: tenant3-pooler
  namespace: $NAMESPACE
  annotations:
    pg-proxy.internal/hostname: tenant3.db.test
    pg-proxy.internal/cluster: tenant3
spec:
  cluster: {name: tenant1-db}
  instances: 1
  type: rw
  pgbouncer: {poolMode: transaction}
EOF
client_exec "grep -q tenant3.db.test /etc/hosts || echo '$apisix_ip tenant3.db.test' >> /etc/hosts"
kubectl rollout status deployment -l cnpg.io/poolerName=tenant3-pooler -n "$NAMESPACE" --timeout=60s >/dev/null 2>&1
# The Pooler Deployment reporting "rolled out" doesn't guarantee PgBouncer
# has finished its own bootstrap (auth_query wiring etc.) yet — poll the
# actual connection instead of a fixed guess at how long that takes.
out7=""
for _ in $(seq 1 10); do
	out7="$(client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant3.db.test port=5432 dbname=app user=app_rw sslmode=verify-full' -tAc 'select 1'" 2>&1)"
	[[ "$out7" == "1" ]] && break
	sleep 2
done
if [[ "$out7" == "1" ]]; then
	record 7 PASS "a newly-annotated Pooler became reachable without restarting pg-proxy"
else
	record 7 FAIL "new Pooler tenant3-pooler was not reachable: $out7"
fi

client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant3.db.test port=5432 dbname=app user=app_rw sslmode=verify-full' -c 'select pg_sleep(8)'" >/tmp/pgproxy-e2e-scenario8.log 2>&1 &
scenario8_pid=$!
sleep 2
kubectl delete pooler tenant3-pooler -n "$NAMESPACE" >/dev/null 2>&1
sleep 3
out8_new="$(client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant3.db.test port=5432 dbname=app user=app_rw sslmode=verify-full connect_timeout=5' -c 'select 1'" 2>&1)"
wait "$scenario8_pid" 2>/dev/null
old8_ok=0
grep -q "pg_sleep" /tmp/pgproxy-e2e-scenario8.log 2>/dev/null && old8_ok=1
if [[ "$out8_new" == *"unknown host"* ]] && [[ "$old8_ok" == 1 ]]; then
	record 8 PASS "after deleting the Pooler: new connections rejected (unknown host), the existing one ran to completion"
else
	record 8 FAIL "out8_new=$out8_new old8_ok=$old8_ok"
fi

# --- Scenario 9: rollout restart must not interrupt existing connections ---
# Targets one specific pod directly (bypassing APISIX's round robin) so
# the query is deterministically running on the exact replica the delete
# below terminates. Duration is deliberately well under shutdown.drain
# Timeout (30s in this environment's ConfigMap) — drainTimeout is a grace
# *bound*, not an unlimited wait, so a query longer than it would be
# force-closed by design, not by a bug (this is why the first version of
# this scenario, with a 40s sleep, looked like a false failure).
scenario9_pod="$(kubectl get pods -n "$NAMESPACE" -l app=pg-proxy -o jsonpath='{.items[0].metadata.name}')"
scenario9_ip="$(kubectl get pod "$scenario9_pod" -n "$NAMESPACE" -o jsonpath='{.status.podIP}')"
client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant1.db.test hostaddr=$scenario9_ip port=5432 dbname=app user=app_rw sslmode=verify-full' -c 'select pg_sleep(10)'" >/tmp/pgproxy-e2e-scenario9.log 2>&1 &
scenario9_pid=$!
sleep 2
kubectl delete pod "$scenario9_pod" -n "$NAMESPACE" >/dev/null
wait "$scenario9_pid" 2>/dev/null
kubectl rollout status deployment/pg-proxy -n "$NAMESPACE" --timeout=90s >/dev/null 2>&1
if grep -q "pg_sleep" /tmp/pgproxy-e2e-scenario9.log 2>/dev/null; then
	record 9 PASS "a connection survived its own pod being deleted mid-query (graceful drain, not instant teardown)"
else
	record 9 FAIL "the connection did not complete when its pod was deleted: $(cat /tmp/pgproxy-e2e-scenario9.log 2>/dev/null)"
fi

# --- Scenario 10: Ctrl-C (CancelRequest) actually cancels a running query ---
# KNOWN LIMITATION (design §12 item 4, found by this e2e suite, not
# assumed away): libpq 17's cancel connection does not resend the
# original SNI, so pg-proxy's Router.Lookup("") misses and the cancel is
# silently dropped as unknown_sni — confirmed independent of APISIX/TLS
# (bypassing both reproduces identically; a direct PgBouncer/Postgres
# connection cancels normally). This scenario runs for real and reports
# the actual outcome rather than asserting the hoped-for one.
sni_before="$(sum_unknown_sni)"
client_exec "pkill -9 psql 2>/dev/null; true" >/dev/null 2>&1
client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant1.db.test port=5432 dbname=app user=app_rw sslmode=verify-full' -c 'select pg_sleep(15)' & QPID=\$!; sleep 3; kill -INT \$QPID; wait \$QPID" >/tmp/pgproxy-e2e-scenario10.log 2>&1
sni_after="$(sum_unknown_sni)"
if grep -qi "canceling statement" /tmp/pgproxy-e2e-scenario10.log 2>/dev/null; then
	record 10 PASS "Ctrl-C cancelled the running query (server reported canceling statement due to user request)"
elif [[ "$sni_after" -gt "$sni_before" ]]; then
	record 10 FAIL "KNOWN LIMITATION (design §12 item 4): cancel connection missed SNI routing (unknown_sni $sni_before -> $sni_after), query ran to completion uncancelled"
else
	record 10 FAIL "query did not appear cancelled and no unknown_sni increment observed: $(cat /tmp/pgproxy-e2e-scenario10.log 2>/dev/null)"
fi

# --- Scenario 11: connections_active returns to 0 after closing everything ---
# sum_active's own scrape (port-forward + curl per pod, ~1.5s x 3 pods)
# takes long enough that a short-lived query could finish mid-scrape and
# be undercounted — pg_sleep's duration must comfortably exceed that
# overhead, not just the initial settle delay.
before="$(sum_active)"
for i in 1 2 3; do
	client_exec "export PGSSLROOTCERT=/etc/e2e-ca/testca.crt PGPASSWORD='$PW1'; psql 'host=tenant1.db.test port=5432 dbname=app user=app_rw sslmode=verify-full' -c 'select pg_sleep(15)'" >/dev/null 2>&1 &
done
sleep 3
during="$(sum_active)"
wait
sleep 1
after="$(sum_active)"
if [[ "$during" -ge $((before + 3)) ]] && [[ "$after" == "$before" ]]; then
	record 11 PASS "connections_active: $before -> $during (3 open) -> $after (all closed) — no leak"
else
	record 11 FAIL "connections_active: before=$before during=$during after=$after"
fi

# --- Scenario 12: sslmode=disable gets an explicit ErrorResponse ---
out12="$(client_exec "psql 'host=tenant1.db.test port=5432 dbname=app user=app_rw sslmode=disable connect_timeout=5' -c 'select 1'" 2>&1)"
if [[ "$out12" == *"only accepts TLS"* ]]; then
	record 12 PASS "sslmode=disable rejected with an explicit ErrorResponse, not a silent close"
else
	record 12 FAIL "expected an explicit TLS-required ErrorResponse, got: $out12"
fi

echo
echo "===== e2e summary: $pass passed, $fail failed ====="
for r in "${results[@]}"; do
	IFS='|' read -r n status detail <<<"$r"
	printf '  [%s] #%s %s\n' "$status" "$n" "$detail"
done

[[ "$fail" -eq 0 ]]
