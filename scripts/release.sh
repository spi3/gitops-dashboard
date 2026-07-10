#!/usr/bin/env bash
set -euo pipefail
script="${BASH_SOURCE[0]}"
while [ -L "$script" ]; do
  dir="$(cd -P "$(dirname "$script")" && pwd -P)"
  target="$(readlink "$script")"
  if [[ "$target" = /* ]]; then script="$target"; else script="$dir/$target"; fi
done
script_dir="$(cd -P "$(dirname "$script")" && pwd -P)"
root="$(cd "$script_dir/.." && pwd -P)"
cd "$root"
unset GODEBUG
exec go run -buildvcs=false ./cmd/release "$@"
