#!/usr/bin/env bash
# setup.sh — installs the full pg-proxy e2e environment (design §11) into
# the pgproxy-e2e namespace: two single-instance CNPG Clusters + Poolers,
# pg-proxy itself (Deployment/Service/ConfigMap/Secret/PDB/RBAC), APISIX
# running **file-driven standalone** (no etcd, no ingress controller — the
# single L4 stream route is baked into deploy/apisix-values.yaml's
# `apisix.deployment.standalone.config` and reloaded by APISIX itself
# polling that file; see README's "APISIX standalone" section for why —
# this matches the company's actual production APISIX topology, task 33),
# and a test client pod with hostAliases resolving
# tenant1.db.test/tenant2.db.test to APISIX's Service.
#
# Fully declarative and idempotent: every step is `kubectl apply` or
# `helm install`-if-absent/`helm upgrade`-if-present; re-running converges
# to the same state rather than erroring or duplicating anything.
#
# Does NOT touch cnpg-system, training, or booking-system — everything
# this script creates lives in pgproxy-e2e plus a single cluster-scoped
# helm release (apisix — no separate ingress-controller release needed
# anymore).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAMESPACE=pgproxy-e2e
IMAGE=pg-proxy:dev

log() { echo "[setup] $*" >&2; }

log "building pg-proxy image ($IMAGE)"
docker build --platform linux/arm64 -t "$IMAGE" "$ROOT" >/tmp/pgproxy-e2e-build.log 2>&1 \
	|| { log "image build failed, see /tmp/pgproxy-e2e-build.log"; exit 1; }

log "generating test TLS material (scripts/gen-certs.sh, no-op if already present)"
bash "$ROOT/scripts/gen-certs.sh"

log "applying namespace, RBAC, CNPG Clusters, Poolers"
kubectl apply -f "$ROOT/deploy/00-namespace.yaml"
kubectl apply -f "$ROOT/deploy/01-rbac.yaml"
kubectl apply -f "$ROOT/deploy/02-clusters.yaml"
kubectl apply -f "$ROOT/deploy/03-poolers.yaml"

log "RBAC pre-flight check (design §7.5 — a wrong Role fails silently as an empty route table, not an error)"
if ! kubectl auth can-i watch poolers.postgresql.cnpg.io \
	--as="system:serviceaccount:${NAMESPACE}:pg-proxy" -n "$NAMESPACE" >/dev/null; then
	log "RBAC check FAILED: pg-proxy's ServiceAccount cannot watch Poolers"
	exit 1
fi
if ! kubectl auth can-i list poolers.postgresql.cnpg.io \
	--as="system:serviceaccount:${NAMESPACE}:pg-proxy" -n "$NAMESPACE" >/dev/null; then
	log "RBAC check FAILED: pg-proxy's ServiceAccount cannot list Poolers"
	exit 1
fi
log "RBAC check OK"

log "waiting for both CNPG Clusters to become healthy"
kubectl wait --for=jsonpath='{.status.phase}'="Cluster in healthy state" \
	cluster/tenant1-db cluster/tenant2-db -n "$NAMESPACE" --timeout=300s

log "waiting for both Poolers to have a Ready replica"
kubectl rollout status deployment -l cnpg.io/poolerName=tenant1-pooler -n "$NAMESPACE" --timeout=120s
kubectl rollout status deployment -l cnpg.io/poolerName=tenant2-pooler -n "$NAMESPACE" --timeout=120s

log "creating pg-proxy's client-facing TLS Secret from wildcard-v1"
kubectl create secret tls pg-proxy-tls -n "$NAMESPACE" \
	--cert="$ROOT/certs/wildcard-v1.crt" --key="$ROOT/certs/wildcard-v1.key" \
	--dry-run=client -o yaml | kubectl apply -f -

log "bundling both tenants' CNPG-generated CAs into pg-proxy's backend.caFile ConfigMap"
tmp_bundle="$(mktemp)"
kubectl get secret tenant1-db-ca -n "$NAMESPACE" -o jsonpath='{.data.ca\.crt}' | base64 -d >"$tmp_bundle"
kubectl get secret tenant2-db-ca -n "$NAMESPACE" -o jsonpath='{.data.ca\.crt}' | base64 -d >>"$tmp_bundle"
kubectl create configmap pg-proxy-backend-ca -n "$NAMESPACE" \
	--from-file=ca.crt="$tmp_bundle" --dry-run=client -o yaml | kubectl apply -f -
rm -f "$tmp_bundle"

log "applying pg-proxy ConfigMap, Deployment, Service, PDB"
kubectl apply -f "$ROOT/deploy/04-pgproxy-configmap.yaml"
kubectl apply -f "$ROOT/deploy/05-pgproxy-deployment.yaml"
# PodMonitor requires the Prometheus Operator CRD, which this environment
# doesn't have installed — apply everything else in the file and skip it
# rather than failing setup over an optional observability resource.
python3 - "$ROOT/deploy/06-pgproxy-service.yaml" <<'PYEOF' | kubectl apply -f -
import sys
with open(sys.argv[1]) as f:
    docs = f.read().split("\n---\n")
print("\n---\n".join(d for d in docs if "kind: PodMonitor" not in d))
PYEOF
if ! kubectl get crd podmonitors.monitoring.coreos.com >/dev/null 2>&1; then
	log "Prometheus Operator not installed — skipping PodMonitor (deploy/06-pgproxy-service.yaml has it for a real cluster)"
else
	kubectl apply -f "$ROOT/deploy/06-pgproxy-service.yaml"
fi

log "waiting for pg-proxy's 3 replicas to become Ready"
kubectl rollout status deployment/pg-proxy -n "$NAMESPACE" --timeout=120s

log "installing/upgrading APISIX (helm)"
helm repo add apisix https://charts.apiseven.com >/dev/null 2>&1 || true
helm repo update apisix >/dev/null
if helm status apisix -n "$NAMESPACE" >/dev/null 2>&1; then
	helm upgrade apisix apisix/apisix -n "$NAMESPACE" -f "$ROOT/deploy/apisix-values.yaml" --wait --timeout 5m
else
	helm install apisix apisix/apisix -n "$NAMESPACE" -f "$ROOT/deploy/apisix-values.yaml" --wait --timeout 5m
fi

log "deploying the e2e test client pod (hostAliases + CA ConfigMap)"
apisix_ip="$(kubectl get svc apisix-gateway -n "$NAMESPACE" -o jsonpath='{.spec.clusterIP}')"
kubectl create configmap e2e-testca -n "$NAMESPACE" \
	--from-file=testca.crt="$ROOT/certs/testca.crt" \
	--from-file=rogueca.crt="$ROOT/certs/rogueca.crt" \
	--dry-run=client -o yaml | kubectl apply -f -
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: e2e-client
  namespace: ${NAMESPACE}
spec:
  restartPolicy: Never
  hostAliases:
    - ip: "${apisix_ip}"
      hostnames: ["tenant1.db.test", "tenant2.db.test"]
  containers:
    - name: psql
      image: postgres:17-alpine
      command: ["sleep", "7200"]
      volumeMounts:
        - name: ca
          mountPath: /etc/e2e-ca
  volumes:
    - name: ca
      configMap:
        name: e2e-testca
EOF
kubectl wait --for=condition=Ready pod/e2e-client -n "$NAMESPACE" --timeout=60s

log "setup complete."
