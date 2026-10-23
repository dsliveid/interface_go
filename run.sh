#!/usr/bin/env sh
# 接口工具 —— 开发期启动脚本（直接用 go run 跑源码，不产出 exe）
#
# 用法：
#   ./run.sh                  启动本地服务并自动拉起 Edge/Chrome 独立窗口
#   ./run.sh -no-browser      只启动服务，手动用浏览器访问
#   ./run.sh -port 9527       固定端口（被占用时自动改用随机端口）
#   ./run.sh -data ./tmpdata  指定数据目录
#   ./run.sh -version         打印版本号后立即退出
#
# 说明：go run 生成的临时 exe 会被 main.go 识别并回退到当前目录，
#       因此开发期数据默认落在本仓库根目录的 data/ 下，与生产行为一致。

set -eu

cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
    echo "[run] 错误：未找到 go 命令，请安装 Go 1.25+ 并加入 PATH。" >&2
    exit 1
fi

echo "[run] $(go version)"
echo "[run] 源码目录：$(pwd)"
echo "[run] 数据目录：$(pwd)/data（可用 -data 覆盖）"
echo "[run] 按 Ctrl+C 结束服务"
echo

# 全部参数透传给程序本体，例如 ./run.sh -port 9527 -no-browser
exec go run . "$@"
