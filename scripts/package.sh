#!/usr/bin/env bash
set -euo pipefail

# Build and package the Linux/amd64 c-shared plugin. Vision-model routing and
# credentials are host-owned; this script never reads or writes credentials.
repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
version="${VERSION:-0.1.1}"
dist_dir="${DIST_DIR:-${repo_dir}/dist}"
go_bin="${GO:-go}"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid VERSION: $version" >&2
  exit 2
fi
command -v "$go_bin" >/dev/null 2>&1 || { echo "Go toolchain not found: $go_bin" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required for deterministic ZIP packaging" >&2; exit 2; }
command -v nm >/dev/null 2>&1 || { echo "nm is required to verify exported ABI symbols" >&2; exit 2; }
command -v strings >/dev/null 2>&1 || { echo "strings is required to verify build metadata" >&2; exit 2; }

host_goos="$($go_bin env GOOS)"
host_goarch="$($go_bin env GOARCH)"
if [[ "$host_goos" != "linux" || "$host_goarch" != "amd64" ]]; then
  echo "package.sh requires a Linux amd64 Go host (got ${host_goos}/${host_goarch}); use Dockerfile.plugin for other hosts" >&2
  exit 2
fi

mkdir -p "$dist_dir"
build_dir="$(mktemp -d "${TMPDIR:-/tmp}/deepseek-vision-package.XXXXXX")"
trap 'rm -rf "$build_dir"' EXIT
artifact="$build_dir/deepseek-vision.so"
archive="$dist_dir/deepseek-vision_${version}_linux_amd64.zip"

echo "building ${archive} with $($go_bin version)"
(
  cd "$repo_dir"
  CGO_ENABLED=1 GOOS=linux GOARCH=amd64 GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" \
    "$go_bin" build -buildmode=c-shared -trimpath -buildvcs=false \
    -ldflags="-s -w -buildid= -X main.pluginVersion=${version}" \
    -o "$artifact" .
)

[[ -s "$artifact" ]] || { echo "compiler produced an empty artifact" >&2; exit 1; }
symbols_file="$build_dir/nm.txt"
nm -D "$artifact" >"$symbols_file"
grep -Eq '[[:space:]]cliproxy_plugin_init$' "$symbols_file" || {
  echo "artifact is missing cliproxy_plugin_init" >&2
  exit 1
}
strings_file="$build_dir/strings.txt"
strings "$artifact" >"$strings_file"
grep -F -- "$version" "$strings_file" >/dev/null || {
  echo "artifact does not contain requested version metadata: $version" >&2
  exit 1
}

# Guard against accidental local captures or credentials in checked-in files.
if rg -n --hidden -g '!.git/**' -g '!dist/**' -g '!*.so' \
  -e 'sk-[A-Za-z0-9]{12,}' -e 'Bearer[[:space:]]+[A-Za-z0-9._-]{16,}' "$repo_dir" >/dev/null; then
  echo "possible credential material found in repository; refusing to package" >&2
  exit 1
fi

VERSION="$version" ARTIFACT="$artifact" ARCHIVE="$archive" python3 - <<'PY'
import hashlib
import os
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile, ZipInfo

artifact = Path(os.environ["ARTIFACT"])
archive = Path(os.environ["ARCHIVE"])
version = os.environ["VERSION"]
payload = artifact.read_bytes()
digest = hashlib.sha256(payload).hexdigest()
checksums = f"{digest}  deepseek-vision.so\n"

archive.parent.mkdir(parents=True, exist_ok=True)
fixed_date = (2020, 1, 1, 0, 0, 0)
with ZipFile(archive, "w", compression=ZIP_DEFLATED, compresslevel=9) as zf:
    for name, data, mode in (
        ("deepseek-vision.so", payload, 0o755),
        ("checksums.txt", checksums.encode("ascii"), 0o644),
    ):
        info = ZipInfo(name, date_time=fixed_date)
        info.compress_type = ZIP_DEFLATED
        info.create_system = 3
        info.external_attr = mode << 16
        info.comment = f"deepseek-vision {version}".encode("ascii") if name.endswith(".so") else b""
        zf.writestr(info, data)
print(f"packaged {archive} ({archive.stat().st_size} bytes)")
PY

(cd "$dist_dir" && sha256sum "$(basename "$archive")") > "${dist_dir}/checksums.txt"
echo "wrote ${dist_dir}/checksums.txt"
