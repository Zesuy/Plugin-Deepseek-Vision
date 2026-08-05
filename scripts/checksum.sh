#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
dist_dir="${DIST_DIR:-${repo_dir}/dist}"
version="${VERSION:-0.1.1}"
archive="${dist_dir}/deepseek-vision_${version}_linux_amd64.zip"
checksums="${dist_dir}/checksums.txt"

[[ -f "$archive" ]] || { echo "archive not found: $archive" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 2; }
actual="$(cd "$dist_dir" && sha256sum "$(basename "$archive")")"
if [[ ! -f "$checksums" ]]; then
  printf '%s\n' "$actual" > "$checksums"
else
  expected="$(awk -v file="$(basename "$archive")" '$2 == file {print; exit}' "$checksums")"
  [[ -n "$expected" ]] || { echo "no checksum entry for $(basename "$archive")" >&2; exit 1; }
  [[ "$expected" == "$actual" ]] || {
    echo "checksum mismatch for $(basename "$archive")" >&2
    echo "expected: $expected" >&2
    echo "actual:   $actual" >&2
    exit 1
  }
fi
echo "$actual"
