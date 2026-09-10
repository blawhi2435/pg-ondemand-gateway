#!/usr/bin/env bash
# teardown.sh — removes everything scripts/setup.sh created: the
# pgproxy-e2e namespace (which cascades to every k8s object inside it —
# CNPG Clusters/Poolers, pg-proxy, the e2e client pod, secrets, configmaps)
# and the apisix helm release (file-driven standalone, task 33 — no
# ingress-controller release exists to uninstall anymore). Does NOT touch
# cnpg-system, training, or booking-system — those live in different
# namespaces entirely and this script never references them.
#
# Also removes the apisix.apache.org/* and gateway.networking.k8s.io/*
# CRDs (plus the Gateway API's ValidatingAdmissionPolicy pair) installed
# cluster-wide during round 6's apisix-ingress-controller 2.x / Gateway
# API investigation — confirmed absent from the cluster before that
# investigation started, and no longer needed now that L4 routing is
# purely file-driven with no controller involved. Unlike round 5/6 (where
# these were left in place because a controller might still need them),
# this teardown removes them since nothing in the current architecture
# uses them.
set -uo pipefail

NAMESPACE=pgproxy-e2e

log() { echo "[teardown] $*" >&2; }

log "uninstalling the apisix helm release (if present)"
helm uninstall apisix -n "$NAMESPACE" >/dev/null 2>&1 || log "apisix release not found, skipping"

log "deleting namespace $NAMESPACE (cascades to every resource inside it)"
kubectl delete namespace "$NAMESPACE" --ignore-not-found=true --wait=true --timeout=180s

log "removing CRDs installed for the round-6 controller/Gateway API investigation (no longer needed)"
crds="$(kubectl get crd -o name 2>/dev/null | grep -E '\.(apisix\.apache\.org|gateway\.networking\.k8s\.io)$' || true)"
if [[ -n "$crds" ]]; then
	# shellcheck disable=SC2086
	kubectl delete $crds --ignore-not-found=true --wait=true --timeout=60s
else
	log "  none found, skipping"
fi
kubectl delete validatingadmissionpolicybinding safe-upgrades.gateway.networking.k8s.io --ignore-not-found=true 2>&1
kubectl delete validatingadmissionpolicy safe-upgrades.gateway.networking.k8s.io --ignore-not-found=true 2>&1

log "verifying pre-existing namespaces were not touched"
for ns in cnpg-system training booking-system; do
	if kubectl get namespace "$ns" >/dev/null 2>&1; then
		log "  $ns: present (untouched, as expected)"
	else
		log "  $ns: MISSING — this should never happen; teardown.sh never references this namespace"
	fi
done

log "teardown complete."
