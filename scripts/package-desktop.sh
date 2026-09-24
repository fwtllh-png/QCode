#!/bin/sh
# 构建 QCode macOS 桌面应用发布产物（QCode.app + zip + 校验和 + 清单）。
#
# 与 scripts/package-release.sh 平行、互不修改对方产物：
#   - 本脚本产出 dist/release/QCode.app 与 QCode-<VERSION>-macos.zip；
#   - 校验和追加进 dist/release/SHA256SUMS，产物条目登记进
#     dist/release/package-manifest.json（存在则合并，不存在则新建）。
#
# 签名与公证按环境变量门控，缺省时明确降级并输出提示（不隐藏环境受限）：
#   - APPLE_DEVELOPER_IDENTITY     Developer ID 证书名（codesign 用）
#   - APPLE_NOTARY_PROFILE         notarytool 的 keychain profile 名
#   - 或 APPLE_ID/APPLE_PASSWORD/APPLE_TEAM_ID 三件套（首选 profile 方式）
#
# 用法: VERSION=0.1.0 RELEASE_STAGE=experimental ./scripts/package-desktop.sh

set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$REPO_ROOT"

VERSION="${VERSION:-}"
RELEASE_STAGE="${RELEASE_STAGE:-experimental}"
PACKAGE_OUT="${PACKAGE_OUT:-dist/release}"
GO=${GO:-go}
MAKE=${MAKE:-make}

if [ -z "$VERSION" ]; then
  echo "package-desktop.sh: VERSION is required" >&2
  exit 2
fi
case "$RELEASE_STAGE" in
  experimental|preview|candidate|default) ;;
  *) echo "package-desktop.sh: invalid RELEASE_STAGE: $RELEASE_STAGE" >&2; exit 2 ;;
esac

COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || printf unknown)}
BUILD_DATE=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
MODULE=github.com/fwtllh-png/QCode
LDFLAGS="-s -w \
	-X $MODULE/internal/buildinfo.Version=$VERSION \
	-X $MODULE/internal/buildinfo.Commit=$COMMIT \
	-X $MODULE/internal/buildinfo.Date=$BUILD_DATE"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/qcode-desktop-pkg.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

echo "==> 构建 Web 前端"
"$MAKE" web-build

echo "==> 构建 universal qcode Runtime"
for arch in arm64 amd64; do
	CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" "$GO" build -tags webbundle -trimpath \
		-ldflags "$LDFLAGS" -o "$WORK/qcode-$arch" ./cmd/qcode
done
lipo -create "$WORK/qcode-arm64" "$WORK/qcode-amd64" -output "$WORK/qcode-universal"
cp "$WORK/qcode-universal" "$WORK/qcode"
"$WORK/qcode" --version

echo "==> 组装 QCode.app"
VERSION="$VERSION" COMMIT="$COMMIT" BUILD_DATE="$BUILD_DATE" \
	./desktop/build-app.sh --binary "$WORK/qcode" --output "$PACKAGE_OUT/QCode.app"
APP="$PACKAGE_OUT/QCode.app"

SIGNED=0
if [ -n "${APPLE_DEVELOPER_IDENTITY:-}" ]; then
	SIGNED=1
else
	echo "package-desktop.sh: 未设置 APPLE_DEVELOPER_IDENTITY，产出未正式签名（不可对外分发）" >&2
fi

NOTARIZED=0
if [ "$SIGNED" = 1 ] && { [ -n "${APPLE_NOTARY_PROFILE:-}" ] || [ -n "${APPLE_ID:-}" ]; }; then
	echo "==> 公证 (notarytool)"
	ZIP_SUBMIT="$WORK/QCode-submit.zip"
	ditto -c -k --keepParent "$APP" "$ZIP_SUBMIT"
	if [ -n "${APPLE_NOTARY_PROFILE:-}" ]; then
		xcrun notarytool submit "$ZIP_SUBMIT" --keychain-profile "$APPLE_NOTARY_PROFILE" --wait
	else
		xcrun notarytool submit "$ZIP_SUBMIT" \
			--apple-id "$APPLE_ID" --password "$APPLE_PASSWORD" --team-id "$APPLE_TEAM_ID" --wait
	fi
	xcrun stapler staple "$APP"
	NOTARIZED=1
else
	echo "package-desktop.sh: 未配置公证凭据，跳过 notarization（分发给他人时会被 Gatekeeper 拦截）" >&2
fi

echo "==> 打包与校验和"
mkdir -p "$PACKAGE_OUT"
ZIP="$PACKAGE_OUT/QCode-$VERSION-macos.zip"
rm -f "$ZIP"
ditto -c -k --keepParent "$APP" "$ZIP"

SUMS="$PACKAGE_OUT/SHA256SUMS"
for artifact in "$ZIP" "$APP/Contents/MacOS/QCode" "$APP/Contents/MacOS/qcode-runtime"; do
	name=$(basename "$artifact")
	digest=$(shasum -a 256 "$artifact" | cut -d' ' -f1)
	if grep -q "  $name\$" "$SUMS" 2>/dev/null; then
		sed -i '' "s|^[0-9a-f]*  $name\$|$digest  $name|" "$SUMS"
	else
		printf '%s  %s\n' "$digest" "$name" >> "$SUMS"
	fi
done

echo "==> 登记清单"
APP_DIGEST=$(shasum -a 256 "$ZIP" | cut -d' ' -f1)
python3 - "$PACKAGE_OUT/package-manifest.json" "$VERSION" "$RELEASE_STAGE" "$APP_DIGEST" "$SIGNED" "$NOTARIZED" <<'PY'
import json
import os
import sys

path, version, stage, digest, signed, notarized = sys.argv[1:7]
entry = {
    "kind": "desktop-app",
    "file": f"QCode-{version}-macos.zip",
    "sha256": digest,
    "signed": signed == "1",
    "notarized": notarized == "1",
}
manifest = {}
if os.path.exists(path):
    with open(path, encoding="utf-8") as handle:
        try:
            manifest = json.load(handle)
        except json.JSONDecodeError:
            manifest = {}
if not isinstance(manifest, dict):
    manifest = {}
artifacts = manifest.setdefault("desktop_artifacts", [])
artifacts[:] = [item for item in artifacts if item.get("file") != entry["file"]]
artifacts.append(entry)
manifest["desktop_schema_version"] = 1
manifest["desktop_version"] = version
manifest["desktop_release_stage"] = stage
with open(path, "w", encoding="utf-8") as handle:
    json.dump(manifest, handle, ensure_ascii=False, indent=2, sort_keys=True)
    handle.write("\n")
PY

echo "==> 完成"
printf '  app:        %s\n' "$APP"
printf '  zip:        %s\n' "$ZIP"
printf '  manifest:   %s\n' "$PACKAGE_OUT/package-manifest.json"
printf '  签名:       %s\n' "$([ "$SIGNED" = 1 ] && printf Developer\ ID || printf adhoc/未签名)"
printf '  公证:       %s\n' "$([ "$NOTARIZED" = 1 ] && printf 已公证 || printf 未公证)"
