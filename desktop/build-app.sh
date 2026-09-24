#!/bin/sh
# 组装 QCode.app 桌面壳：编译 Swift 壳（双架构 + lipo）、嵌入 universal
# qcode Runtime 二进制、生成图标、写 Info.plist、codesign。
#
# 用法: desktop/build-app.sh --binary <qcode 二进制> --output <QCode.app 路径>
# 版本信息经环境变量 VERSION / COMMIT / BUILD_DATE 注入（与 Makefile 约定一致）。
#
# 签名策略（与仓库“报告环境受限而不是隐藏”的原则一致）：
#   - 设置 APPLE_DEVELOPER_IDENTITY 时用 Developer ID + Hardened Runtime 签名；
#   - 否则 adhoc 签名并明确输出提示。

set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
SHELL_SRC="$REPO_ROOT/desktop/QCodeApp"
PLIST_TEMPLATE="$SHELL_SRC/Resources/Info.plist"
ICON_SOURCE="$REPO_ROOT/web/public/icon-512.png"

BINARY=""
OUTPUT=""
VERSION="${VERSION:-dev}"
COMMIT="${COMMIT:-unknown}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y%m%d%H%M%S)}"
MIN_MACOS="13.0"

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BINARY=$2; shift 2 ;;
    --output) OUTPUT=$2; shift 2 ;;
    *) echo "build-app.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$BINARY" ] || [ -z "$OUTPUT" ]; then
  echo "usage: desktop/build-app.sh --binary <qcode binary> --output <QCode.app>" >&2
  exit 2
fi
if [ ! -x "$BINARY" ]; then
  echo "build-app.sh: qcode binary not executable: $BINARY" >&2
  exit 1
fi
for tool in swiftc lipo sips iconutil codesign; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "build-app.sh: missing required tool: $tool" >&2
    exit 1
  fi
done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/qcode-desktop.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

echo "==> 编译 Swift 壳 (macOS $MIN_MACOS)"
# main.swift 提供顶层入口，多文件编译即产出可执行文件，不能用 -parse-as-library。
HAVE_ARM64=0
HAVE_X86_64=0
for arch in arm64 x86_64; do
  if swiftc -target "${arch}-apple-macos${MIN_MACOS}" \
      -O \
      -module-name QCode \
      -o "$WORK/shell-$arch" \
      "$SHELL_SRC"/*.swift 2>"$WORK/swiftc-$arch.log"; then
    case "$arch" in
      arm64) HAVE_ARM64=1 ;;
      x86_64) HAVE_X86_64=1 ;;
    esac
  else
    echo "build-app.sh: swiftc failed for $arch:" >&2
    cat "$WORK/swiftc-$arch.log" >&2
  fi
done
if [ "$HAVE_ARM64" = 1 ] && [ "$HAVE_X86_64" = 1 ]; then
  lipo -create "$WORK/shell-arm64" "$WORK/shell-x86_64" -output "$WORK/QCode"
elif [ "$HAVE_ARM64" = 1 ]; then
  echo "build-app.sh: x86_64 编译不可用，仅产出 arm64 壳" >&2
  cp "$WORK/shell-arm64" "$WORK/QCode"
elif [ "$HAVE_X86_64" = 1 ]; then
  echo "build-app.sh: arm64 编译不可用，仅产出 x86_64 壳" >&2
  cp "$WORK/shell-x86_64" "$WORK/QCode"
else
  echo "build-app.sh: Swift 壳编译失败" >&2
  exit 1
fi

echo "==> 组装 .app"
rm -rf "$OUTPUT"
APP_MACOS="$OUTPUT/Contents/MacOS"
APP_RESOURCES="$OUTPUT/Contents/Resources"
mkdir -p "$APP_MACOS" "$APP_RESOURCES"
cp "$WORK/QCode" "$APP_MACOS/QCode"
chmod 755 "$APP_MACOS/QCode"
# Runtime 不能命名为 qcode：默认 APFS 大小写不敏感，会与壳的可执行文件 QCode 冲突。
cp "$BINARY" "$APP_MACOS/qcode-runtime"
chmod 755 "$APP_MACOS/qcode-runtime"

echo "==> 生成 AppIcon.icns"
ICONSET="$WORK/AppIcon.iconset"
mkdir -p "$ICONSET"
for size in 16 32 128 256 512; do
  sips -z "$size" "$size" "$ICON_SOURCE" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
  double=$((size * 2))
  if [ "$double" -le 1024 ]; then
    sips -z "$double" "$double" "$ICON_SOURCE" --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
  fi
done
iconutil -c icns "$ICONSET" -o "$APP_RESOURCES/AppIcon.icns"

echo "==> 写入 Info.plist"
sed -e "s/__VERSION__/$VERSION/g" -e "s/__BUILD_DATE__/$BUILD_DATE/g" \
  "$PLIST_TEMPLATE" > "$OUTPUT/Contents/Info.plist"

echo "==> codesign"
if [ -n "${APPLE_DEVELOPER_IDENTITY:-}" ]; then
  codesign --force --deep --options runtime --timestamp \
    --sign "$APPLE_DEVELOPER_IDENTITY" "$OUTPUT"
  echo "build-app.sh: 已使用 Developer ID 签名: $APPLE_DEVELOPER_IDENTITY"
else
  codesign --force --deep --sign - "$OUTPUT"
  echo "build-app.sh: 未设置 APPLE_DEVELOPER_IDENTITY，产出为 adhoc 签名（不可分发）"
fi

echo "==> 完成: $OUTPUT"
