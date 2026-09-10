#!/usr/bin/env bash
# gen-certs.sh — generate the test-only TLS material for pg-proxy's e2e
# environment (design §10.3). Produces:
#
#   testca.crt / testca.key         test root CA, stands in for "the
#                                    company CA" in this environment
#   wildcard-v1.crt / .key          *.db.test, signed by testca, pg-proxy's
#                                    initial client-facing certificate
#   wildcard-v2.crt / .key          same subject, different serial — used
#                                    to exercise cert hot reload
#   rogueca.crt / .key              an unrelated CA
#   rogue-wildcard.crt / .key       *.db.test signed by rogueca — a
#                                    negative test: verify-full against
#                                    testca.crt must reject this
#
# Every wildcard cert carries subjectAltName = DNS:*.db.test — modern
# libpq/OpenSSL ignore the CN entirely for hostname verification, so a
# cert with only a CN would make every verify-full test silently pass for
# the wrong reason (or fail outright).
#
# Output goes to $CERT_DIR (default: ./certs, gitignored). Re-running is
# safe: existing files are left alone unless FORCE=1.
set -euo pipefail

CERT_DIR="${CERT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/certs}"
DOMAIN="${PGPROXY_TEST_DOMAIN:-db.test}"
FORCE="${FORCE:-0}"
DAYS=3650

mkdir -p "$CERT_DIR"
cd "$CERT_DIR"

log() { echo "[gen-certs] $*" >&2; }

skip_if_present() {
	local label="$1"
	shift
	if [[ "$FORCE" != "1" ]] && [[ -e "$1" ]]; then
		log "skip $label (already exists; set FORCE=1 to regenerate)"
		return 0
	fi
	return 1
}

gen_ca() {
	local name="$1" cn="$2"
	skip_if_present "$name CA" "$name.crt" && return 0
	log "generating $name CA ($cn)"
	openssl ecparam -name prime256v1 -genkey -noout -out "$name.key"
	openssl req -new -x509 -key "$name.key" -days "$DAYS" \
		-subj "/CN=$cn" -out "$name.crt"
}

# gen_leaf issues a leaf cert signed by the given CA, with
# subjectAltName=DNS:$dns_name — required for verify-full (design §10.3).
gen_leaf() {
	local name="$1" ca_name="$2" cn="$3" dns_name="$4" serial="$5"
	skip_if_present "$name cert" "$name.crt" && return 0
	log "generating $name ($dns_name, signed by $ca_name, serial=$serial)"

	openssl ecparam -name prime256v1 -genkey -noout -out "$name.key"
	openssl req -new -key "$name.key" -subj "/CN=$cn" -out "$name.csr"

	local ext_file
	ext_file="$(mktemp)"
	trap 'rm -f "$ext_file"' RETURN
	cat >"$ext_file" <<-EOF
		subjectAltName = DNS:$dns_name
		extendedKeyUsage = serverAuth
	EOF

	openssl x509 -req -in "$name.csr" \
		-CA "$ca_name.crt" -CAkey "$ca_name.key" -CAcreateserial \
		-set_serial "$serial" -days "$DAYS" -extfile "$ext_file" \
		-out "$name.crt"
	rm -f "$name.csr"
}

gen_ca testca "pg-proxy test root CA"
gen_leaf wildcard-v1 testca "*.${DOMAIN}" "*.${DOMAIN}" 1
gen_leaf wildcard-v2 testca "*.${DOMAIN}" "*.${DOMAIN}" 2

gen_ca rogueca "unrelated rogue CA"
gen_leaf rogue-wildcard rogueca "*.${DOMAIN}" "*.${DOMAIN}" 1

log "done. Artifacts are in $CERT_DIR (gitignored — see .gitignore)."
log "verify-full clients should trust testca.crt via PGSSLROOTCERT."
log "rogue-wildcard.crt / rogueca.crt exist only to prove a verify-full"
log "client rejects a cert from an unrelated CA (design §10.2 scenario 3)."
