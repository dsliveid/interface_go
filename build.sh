#!/usr/bin/env sh
# 接口工具 —— 打包脚本：产出单文件可执行程序（前端资源已 //go:embed 内嵌，
# 拷走一个 exe 即可在隔离环境运行，无需 Node / Rust / 任何前端工具链）。
#
# 用法：
#   ./build.sh                        按当前平台打包，版本号自动附带 git 短哈希
#   ./build.sh -v 1.2.0               指定版本号
#   ./build.sh -o dist/app.exe        指定输出路径（目录不存在会自动创建）
#   ./build.sh --console              保留控制台窗口（调试用；默认 Windows 下隐藏）
#   ./build.sh --strip                剥离符号表与源码路径（产物更小，但可分析性差）
#   ./build.sh --skip-check           跳过 go vet 静态检查，加快打包
#   GOOS=linux GOARCH=arm64 ./build.sh    交叉编译到其他平台
#
# 环境变量：VERSION（版本号）、OUT（输出路径）亦可替代同名参数。
#
# 说明：Windows/amd64 产物默认不剥符号（体积约 10MB），并附带完整的图标、
# 版本信息与应用清单。剥符号后的「不可读高熵二进制」是杀软启发式的典型减分项，
# 用几 MB 体积换可分析性是划算的；确实需要小体积时再加 --strip。

set -eu

cd "$(dirname "$0")"

VERSION="${VERSION:-}"
OUT="${OUT:-}"
CONSOLE=0
SKIP_CHECK=0
STRIP=0

usage() {
    sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
}

need_value() {
    if [ "$1" -lt 2 ]; then
        echo "[build] 错误：参数 $2 缺少取值。" >&2
        exit 1
    fi
}

while [ $# -gt 0 ]; do
    case "$1" in
        -v|--version)
            need_value $# "$1"; VERSION="$2"; shift 2 ;;
        -o|--output)
            need_value $# "$1"; OUT="$2"; shift 2 ;;
        --console)
            CONSOLE=1; shift ;;
        --strip)
            STRIP=1; shift ;;
        --skip-check)
            SKIP_CHECK=1; shift ;;
        -h|--help)
            usage ;;
        *)
            echo "[build] 错误：未知参数 $1（用 --help 查看用法）。" >&2
            exit 1 ;;
    esac
done

if ! command -v go >/dev/null 2>&1; then
    echo "[build] 错误：未找到 go 命令，请安装 Go 1.25+ 并加入 PATH。" >&2
    exit 1
fi

GOOS_VAL="$(go env GOOS)"
GOARCH_VAL="$(go env GOARCH)"

# 版本号优先级：-v 参数 / VERSION 环境变量 > git 短哈希 > 兜底 1.0.0
if [ -z "$VERSION" ]; then
    VERSION="1.0.0"
    if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
        HASH="$(git rev-parse --short HEAD 2>/dev/null || true)"
        if [ -n "$HASH" ]; then
            VERSION="1.0.0+${HASH}"
            if ! git diff --quiet 2>/dev/null; then
                VERSION="${VERSION}-dirty"
            fi
        fi
    fi
fi

if [ -z "$OUT" ]; then
    case "$GOOS_VAL" in
        windows) OUT="接口工具.exe" ;;
        *)       OUT="interface-go" ;;
    esac
fi

OUT_DIR="$(dirname "$OUT")"
[ -d "$OUT_DIR" ] || mkdir -p "$OUT_DIR"

# 默认保留符号表与 DWARF：剥符号后的「不可读高熵二进制」是杀软启发式的典型
# 减分项，用几 MB 体积换取可分析性是划算的。追求小体积时加 --strip。
LDFLAGS="-X main.version=${VERSION}"
BUILD_FLAGS=""
if [ "$STRIP" -eq 1 ]; then
    LDFLAGS="-s -w ${LDFLAGS}"
    BUILD_FLAGS="-trimpath"
fi
if [ "$GOOS_VAL" = "windows" ] && [ "$CONSOLE" -eq 0 ]; then
    # 隐藏控制台窗口，运行起来更像桌面应用（调试时用 --console）
    LDFLAGS="${LDFLAGS} -H=windowsgui"
fi

export CGO_ENABLED=0

echo "[build] $(go version)"
echo "[build] 目标平台：${GOOS_VAL}/${GOARCH_VAL}"
echo "[build] 版本号：${VERSION}"
echo "[build] 输出路径：${OUT}"

# Windows/amd64 才需要资源对象；.syso 文件名带平台后缀，其他平台会被 Go 忽略。
RES_SYSO="rsrc_windows_amd64.syso"
if [ "$GOOS_VAL" = "windows" ] && [ "$GOARCH_VAL" = "amd64" ]; then
    echo "[build] 生成资源对象：图标 + 版本信息 + 应用清单"
    if ! go run ./tools/winres -ico assets/app.ico -out "$RES_SYSO" -version "$VERSION"; then
        if [ -f "$RES_SYSO" ]; then
            echo "[build] 警告：资源生成失败，沿用仓库内既有 $RES_SYSO。" >&2
        else
            echo "[build] 错误：资源生成失败且缺少 $RES_SYSO，无法打包带图标的 Windows 产物。" >&2
            exit 1
        fi
    fi
fi

if [ "$SKIP_CHECK" -eq 0 ]; then
    echo "[build] 静态检查：go vet ./..."
    if ! go vet ./...; then
        echo "[build] go vet 未通过，已中止打包（可加 --skip-check 跳过）。" >&2
        exit 1
    fi
fi

echo "[build] 编译中…"
# BUILD_FLAGS 可能为空，这里必须不加引号以便按空格分词。
if ! go build $BUILD_FLAGS -ldflags "$LDFLAGS" -o "$OUT" .; then
    echo "[build] 编译失败：若提示文件被占用，请先关闭正在运行的 接口工具 再重试。" >&2
    exit 1
fi

if [ ! -f "$OUT" ]; then
    echo "[build] 错误：未找到产物 $OUT。" >&2
    exit 1
fi

# Go 为可重现构建会把 PE 时间戳置 0、校验和留空，这在杀软启发式里等同于
# 「手工拼装/加壳的 PE」。这里补回真实时间戳与规范校验和（须在签名之前执行）。
if [ "$GOOS_VAL" = "windows" ]; then
    if ! go run ./tools/winres -fixexe "$OUT"; then
        echo "[build] 警告：PE 头修正失败，产物仍可运行，但时间戳保持为 0。" >&2
    fi
fi

BYTES="$(wc -c < "$OUT" | tr -d ' ')"
SIZE_MB="$(awk "BEGIN{printf \"%.2f\", ${BYTES}/1048576}")"

echo "[build] 打包完成：${OUT}（${SIZE_MB} MB，${BYTES} 字节）"
echo "[build] 前端资源已内嵌，复制该文件到目标机器即可运行；首次启动会自动创建同级 data/ 目录。"
if [ "$GOOS_VAL" = "windows" ] && [ "$CONSOLE" -eq 0 ]; then
    echo "[build] 提示：无控制台版本看不到 -version / 日志输出，排查问题时请改用 ./build.sh --console。"
fi
