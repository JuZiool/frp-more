#!/usr/bin/env bash
# 构建 fnOS .fpk 安装包
# 用法: ./fnos/build.sh [版本号]      版本号默认取最近一个 git tag（去掉 v 前缀）
# 依赖: go, fnpack (https://developer.fnnas.com/docs/cli/fnpack/)
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//')}"
echo "==> 版本: ${VERSION}"

STAGE="fnos/build"
rm -rf "$STAGE"
mkdir -p "$STAGE"

echo "==> 组装包内容"
cp fnos/manifest "$STAGE/manifest"
sed -i "s/__VERSION__/${VERSION}/" "$STAGE/manifest"
cp -r fnos/config "$STAGE/config"
cp -r fnos/cmd "$STAGE/cmd"
cp -r fnos/app "$STAGE/app"
cp fnos/app/ui/images/icon_64.png "$STAGE/ICON.PNG"
cp fnos/app/ui/images/icon_256.png "$STAGE/ICON_256.PNG"
chmod +x "$STAGE/cmd/main"

echo "==> 交叉编译 linux/amd64"
# 包内 app/ 的内容会安装到应用根目录(TRIM_APPDEST)，二进制放 app/bin/frp-more，
# 对应运行时路径 ${TRIM_APPDEST}/bin/frp-more
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$STAGE/app/bin/frp-more" .

echo "==> fnpack build"
fnpack build --directory "$STAGE"

mv frp-more.fpk "frp-more-${VERSION}.fpk"
echo "==> 完成: frp-more-${VERSION}.fpk"
