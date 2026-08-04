#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
smoke_dir="$(mktemp -d "${TMPDIR:-/tmp}/deepseek-vision-smoke.XXXXXX")"
trap 'rm -rf "$smoke_dir"' EXIT

VERSION="${VERSION:-0.1.1}" DIST_DIR="$smoke_dir/dist" \
  "$repo_dir/scripts/package.sh"
VERSION="${VERSION:-0.1.1}" DIST_DIR="$smoke_dir/dist" \
  "$repo_dir/scripts/checksum.sh"

VERSION="${VERSION:-0.1.1}" DIST_DIR="$smoke_dir/dist" ARCHIVE_ROOT="$smoke_dir/unpacked" \
  python3 - <<'PY'
import hashlib
import os
import re
from pathlib import Path
from zipfile import ZipFile

dist = Path(os.environ["DIST_DIR"])
version = os.environ["VERSION"]
archive = dist / f"deepseek-vision_{version}_linux_amd64.zip"
unpacked = Path(os.environ["ARCHIVE_ROOT"])
with ZipFile(archive) as zf:
    names = zf.namelist()
    if names != ["deepseek-vision.so", "checksums.txt"]:
        raise SystemExit(f"unexpected ZIP members: {names!r}")
    zf.extractall(unpacked)
so = unpacked / "deepseek-vision.so"
if so.stat().st_mode & 0o400 == 0:
    raise SystemExit("plugin artifact is not readable")
line = (unpacked / "checksums.txt").read_text().strip()
want = hashlib.sha256(so.read_bytes()).hexdigest()
if line != f"{want}  deepseek-vision.so":
    raise SystemExit("embedded plugin checksum mismatch")
blob = archive.read_bytes()
if re.search(rb"sk-[A-Za-z0-9]{12,}", blob) or re.search(rb"Bearer\s+[A-Za-z0-9._-]{16,}", blob):
    raise SystemExit("possible credential marker in archive")
print(f"smoke-verified {archive}")
PY
