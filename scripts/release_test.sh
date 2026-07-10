#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go -C "$root" test ./cmd/release
tmp="$(mktemp -d)"
fixture_env="$root/.env.release-test-fixture"
fixture_key="$root/release-test-fixture.key"
fixture_credential="$root/data/release-test-fixture-credential"
fixture_artifact="$root/dist/release-test-fixture-artifact"
trap 'rm -f "$fixture_env" "$fixture_key" "$fixture_credential" "$fixture_artifact"; rmdir "$root/data" "$root/dist" 2>/dev/null || true; rm -rf "$tmp"' EXIT
mkdir -p "$(dirname "$fixture_credential")" "$(dirname "$fixture_artifact")"
printf 'fixture environment\n' > "$fixture_env"
printf 'fixture key\n' > "$fixture_key"
printf 'fixture credential\n' > "$fixture_credential"
printf 'fixture artifact\n' > "$fixture_artifact"
[[ "$(wc -l < "$root/scripts/release.sh")" -le 15 ]]
mkdir "$tmp/repo"
# Build the fixture from tracked and non-ignored files only. Do not use a
# checkout-wide copy: ignored files may contain credentials or runtime state.
manifest="${RELEASE_TEST_FILE_MANIFEST:-}"
if [[ -n "$manifest" ]]; then
  [[ -f "$manifest" ]] || {
    echo "release-test file manifest does not exist: $manifest" >&2
    exit 1
  }
else
  manifest="$tmp/release-test-files.nul"
  if ! (cd "$root" && git ls-files --cached --others --exclude-standard -z > "$manifest"); then
    echo "failed to create release-test file manifest from git" >&2
    exit 1
  fi
fi
if [[ ! -s "$manifest" ]]; then
  echo "release-test file manifest is empty" >&2
  exit 1
fi

copied_entries=0
while IFS= read -r -d '' path; do
  mkdir -p "$tmp/repo/$(dirname "$path")"
  cp -a "$root/$path" "$tmp/repo/$path"
  ((copied_entries += 1))
done < "$manifest"
if ((copied_entries == 0)); then
  echo "release-test file manifest contained no entries" >&2
  exit 1
fi
[[ ! -e "$tmp/repo/${fixture_env#"$root/"}" ]]
[[ ! -e "$tmp/repo/${fixture_key#"$root/"}" ]]
[[ ! -e "$tmp/repo/${fixture_credential#"$root/"}" ]]
[[ ! -e "$tmp/repo/${fixture_artifact#"$root/"}" ]]
[[ ! -e "$tmp/repo/data/github_pat.txt" ]]
git -C "$tmp/repo" init -b main >/dev/null
git -C "$tmp/repo" add --all
git -C "$tmp/repo" -c user.name=test -c user.email=test@example.invalid commit -m 'release test fixture' >/dev/null
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go -C "$tmp/repo" build -o "$tmp/release" ./cmd/release
marker="$tmp/release-sentinel-ran"
printf '#!/usr/bin/env bash\ntouch %q\nexit 99\n' "$marker" > "$tmp/repo/release"
chmod 700 "$tmp/repo/release"
# This must remain untouched: go run must not perform build-VCS inspection
# before the release binary's own repository-config guard runs.
fsmonitor="$tmp/repo/fsmonitor"
printf '#!/usr/bin/env bash\ntouch %q\n' "$marker" > "$fsmonitor"
chmod 700 "$fsmonitor"
git -C "$tmp/repo" config remote.origin.url https://github.com/acme/repo.git
git -C "$tmp/repo" config core.fsmonitor "$fsmonitor"
set +e
(cd "$tmp" && GOCACHE=/tmp/gitops-dashboard-go-cache "$tmp/repo/scripts/release.sh" minor >"$tmp/out" 2>&1)
status=$?
set -e
[[ $status -ne 0 ]]
[[ ! -e "$marker" ]]
grep -F "core.fsmonitor" "$tmp/out" >/dev/null
ln -s "$tmp/repo/scripts/release.sh" "$tmp/release-link"
set +e
(cd "$tmp" && GOCACHE=/tmp/gitops-dashboard-go-cache "$tmp/release-link" minor >"$tmp/symlink-out" 2>&1)
status=$?
set -e
[[ $status -ne 0 ]]
grep -F "core.fsmonitor" "$tmp/symlink-out" >/dev/null
! grep -F "go.mod file not found" "$tmp/symlink-out" >/dev/null
set +e
(cd "$tmp" && GOCACHE=/tmp/gitops-dashboard-go-cache "$tmp/repo/scripts/release.sh" --root "$tmp" minor >"$tmp/root-out" 2>&1)
status=$?
set -e
[[ $status -ne 0 ]]
grep -F "flag provided but not defined: -root" "$tmp/root-out" >/dev/null
secret='token-never-on-stderr'
set +e
(cd "$tmp/repo" && GODEBUG="http2debug=2,$secret" "$tmp/release" minor >"$tmp/godebug-out" 2>&1)
status=$?
set -e
[[ $status -ne 0 ]]
! grep -F "$secret" "$tmp/godebug-out" >/dev/null
grep -F "GODEBUG HTTP debugging is not permitted" "$tmp/godebug-out" >/dev/null
# The wrapper independently removes GODEBUG before starting its binary.
set +e
(cd "$tmp" && GOCACHE=/tmp/gitops-dashboard-go-cache GODEBUG="http2debug=2,$secret" "$tmp/repo/scripts/release.sh" minor >"$tmp/wrapper-godebug-out" 2>&1)
status=$?
set -e
[[ $status -ne 0 ]]
! grep -F "GODEBUG HTTP debugging is not permitted" "$tmp/wrapper-godebug-out" >/dev/null
! grep -F "$secret" "$tmp/wrapper-godebug-out" >/dev/null
echo "release binary clean-clone invocation passed"
