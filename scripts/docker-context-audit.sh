#!/bin/sh
# Defense-in-depth audit of the BuildKit-filtered build context. Docker has
# already applied .dockerignore before this script ever sees the context, so
# a positive "forbidden path" finding here means .dockerignore itself failed;
# this script cannot detect content .dockerignore already excluded, only
# content that reached the (already-filtered) context. It fails the build
# before any local COPY/ADD if either check below finds something.
set -eu

context_dir="${1:?context directory required}"

forbidden_found=0
for pattern in \
	data \
	.env '.env.*' \
	.kube kubeconfig 'kubeconfig.*' \
	id_rsa id_dsa id_ecdsa id_ed25519 \
	'*.db' '*.db-shm' '*.db-wal' \
	'*.sqlite' '*.sqlite-shm' '*.sqlite-wal' \
	'*.sqlite3' '*.sqlite3-shm' '*.sqlite3-wal' \
	'*.key' '*.pem' '*.p12' '*.pfx'
do
	if find "$context_dir" -name "$pattern" 2>/dev/null | grep -q .; then
		echo "docker-context-audit: forbidden path class '$pattern' present in build context" >&2
		forbidden_found=1
	fi
done
if [ "$forbidden_found" -ne 0 ]; then
	exit 1
fi
echo "docker-context-audit: forbidden path classes absent"

# A generic PEM private-key marker. This is a detection pattern, not a
# secret: it never contains real key material, and it must never be treated
# as one when logged. Any file bearing it inside the (already-filtered)
# context is assumed to be a locally added private key, even if that file
# would otherwise be allowed by .dockerignore.
#
# The marker is assembled from two halves, kept apart here so this script's
# own on-disk content (which the recursive grep below also scans, since
# scripts/docker-context-audit.sh is itself an allowed context file) never
# contains the full contiguous marker and therefore never matches itself.
marker_head='-----BEGIN'
marker_tail='PRIVATE KEY-----'
marker="$marker_head $marker_tail"
if grep -R -l -- "$marker" "$context_dir" >/dev/null 2>&1; then
	echo "docker-context-audit: private-key marker found in allowed build context content" >&2
	exit 1
fi

echo "docker-context-audit: ok"
