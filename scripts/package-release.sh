#!/usr/bin/env bash
# package-release.sh builds hermetic macOS release artifacts.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [[ ! -f web/dist/index.html ]]; then
  echo "web bundle is missing; run make web-build before packaging" >&2
  exit 2
fi

VERSION="${VERSION:-dev}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || printf unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
STAGE="${RELEASE_STAGE:-experimental}"
case "$STAGE" in
  experimental|preview|candidate|default) ;;
  *)
    echo "RELEASE_STAGE must be experimental|preview|candidate|default" >&2
    exit 2
    ;;
esac

OUT="${PACKAGE_OUT:-dist/release}"
rm -rf "$OUT"
mkdir -p "$OUT/bin" "$OUT/sbom" "$OUT/notes"

MODULE="github.com/fwtllh-png/QCode"
LDFLAGS="-s -w -X ${MODULE}/internal/buildinfo.Version=${VERSION} -X ${MODULE}/internal/buildinfo.Commit=${COMMIT} -X ${MODULE}/internal/buildinfo.Date=${BUILD_DATE}"

targets=(
  "darwin amd64"
  "darwin arm64"
)

for pair in "${targets[@]}"; do
  set -- $pair
  goos="$1"
  goarch="$2"
  name="qcode-${VERSION}-${goos}-${goarch}"
  echo "building ${name}"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -tags webbundle -trimpath -ldflags "$LDFLAGS" -o "$OUT/bin/$name" ./cmd/qcode
done

# Host binary for install/upgrade/rollback smoke.
host_name="qcode"
CGO_ENABLED=0 go build -tags webbundle -trimpath -ldflags "$LDFLAGS" -o "$OUT/bin/${host_name}" ./cmd/qcode

(
  cd "$OUT"
  tar -czf "qcode-${VERSION}.tar.gz" bin
)

(
  cd "$OUT"
  : > SHA256SUMS
  while IFS= read -r -d '' file; do
    if command -v shasum >/dev/null 2>&1; then
      shasum -a 256 "$file" >> SHA256SUMS
    else
      sha256sum "$file" >> SHA256SUMS
    fi
  done < <(find . -type f ! -name 'SHA256SUMS' -print0 | sort -z)
)

SBOM="$OUT/sbom/qcode-${VERSION}.cdx.json"
export QCODE_PACKAGE_ROOT="$ROOT"
export QCODE_PACKAGE_VERSION="$VERSION"
export QCODE_PACKAGE_SBOM="$SBOM"
if command -v syft >/dev/null 2>&1; then
  syft "dir:${ROOT}" -o cyclonedx-json > "$SBOM"
else
  python3 - <<'PY'
import json, datetime, pathlib, os
root = pathlib.Path(os.environ["QCODE_PACKAGE_ROOT"])
version = os.environ["QCODE_PACKAGE_VERSION"]
sbom_path = pathlib.Path(os.environ["QCODE_PACKAGE_SBOM"])
components = []
seen = set()
gomod = (root / "go.mod").read_text(encoding="utf-8")
for line in gomod.splitlines():
    line = line.strip()
    if not line or line.startswith("module ") or line.startswith("go ") or line.startswith("require (") or line == ")" or line.startswith("replace ") or line.startswith("//"):
        continue
    if line.startswith("require "):
        line = line[len("require "):].strip()
    parts = line.split()
    if len(parts) < 2:
        continue
    name, ver = parts[0], parts[1]
    if name in seen:
        continue
    seen.add(name)
    components.append({
        "type": "library",
        "name": name,
        "version": ver,
        "purl": f"pkg:golang/{name}@{ver}",
    })
gosum = root / "go.sum"
if gosum.exists():
    for line in gosum.read_text(encoding="utf-8").splitlines():
        parts = line.split()
        if len(parts) < 2:
            continue
        name, ver = parts[0], parts[1]
        if ver.endswith("/go.mod"):
            continue
        key = f"{name}@{ver}"
        if key in seen:
            continue
        seen.add(key)
        components.append({
            "type": "library",
            "name": name,
            "version": ver,
            "purl": f"pkg:golang/{name}@{ver}",
        })
doc = {
    "bomFormat": "CycloneDX",
    "specVersion": "1.5",
    "version": 1,
    "metadata": {
        "timestamp": datetime.datetime.utcnow().replace(microsecond=0).isoformat() + "Z",
        "component": {"type": "application", "name": "qcode", "version": version},
        "tools": [{"name": "go.mod/go.sum fallback", "version": "1"}],
    },
    "components": components,
}
sbom_path.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
print("wrote fallback SBOM with", len(components), "components")
PY
fi

cat > "$OUT/notes/RELEASE_NOTES.md" <<EOF
# QCode ${VERSION}

- Commit: ${COMMIT}
- Built: ${BUILD_DATE}
- Stage: ${STAGE}
- Artifacts: macOS amd64/arm64 binaries, SHA256SUMS, CycloneDX SBOM

## Install smoke

1. Extract tarball
2. Run \`./bin/qcode --version\`
3. Replace binary for upgrade and verify checksums
4. Restore previous checksum-verified binary for rollback
EOF

python3 - <<PY
import json, hashlib, pathlib, datetime
out = pathlib.Path("${OUT}")
sums = (out / "SHA256SUMS").read_text(encoding="utf-8")
digest = hashlib.sha256(sums.encode()).hexdigest()
manifest = {
  "schema_version": 1,
  "product": "qcode",
  "version": "${VERSION}",
  "commit": "${COMMIT}",
  "built_at": "${BUILD_DATE}",
  "stage": "${STAGE}",
  "stage_sequence": ["experimental", "preview", "candidate", "default"],
  "tarball": f"qcode-${VERSION}.tar.gz",
  "sbom": f"sbom/qcode-${VERSION}.cdx.json",
  "checksums": "SHA256SUMS",
  "sha256sums_digest": digest,
  "generated_at": datetime.datetime.utcnow().replace(microsecond=0).isoformat() + "Z",
}
(out / "package-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
print("package ready", out)
PY

# Install / upgrade / rollback smoke against host binary.
smoke_dir="$(mktemp -d)"
cleanup_smoke() { rm -rf "$smoke_dir"; }
trap cleanup_smoke EXIT
install_dir="$smoke_dir/install"
mkdir -p "$install_dir"
cp "$OUT/bin/${host_name}" "$install_dir/qcode"
"$install_dir/qcode" --version >/dev/null
"$install_dir/qcode" --help >/dev/null
cp "$install_dir/qcode" "$smoke_dir/qcode.prev"
# Upgrade overwrite via new inode to avoid macOS text-busy / codesign cache kills.
cp "$OUT/bin/${host_name}" "$install_dir/qcode.upgrade"
mv -f "$install_dir/qcode.upgrade" "$install_dir/qcode"
"$install_dir/qcode" --version >/dev/null
# Rollback restore previous binary and verify size match.
cp "$smoke_dir/qcode.prev" "$install_dir/qcode.rollback"
mv -f "$install_dir/qcode.rollback" "$install_dir/qcode"
test "$(wc -c < "$install_dir/qcode")" -eq "$(wc -c < "$smoke_dir/qcode.prev")"
./scripts/check-brand.sh

echo "package-release completed stage=${STAGE} out=${OUT}"
