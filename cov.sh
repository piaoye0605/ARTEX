#!/usr/bin/env bash
# 覆盖率一键闸门：跑测试 → 生成 profile → 按包排名 → 与阈值比对。
#
#   ./cov.sh                 # 全仓测试 + 默认阈值（65%）
#   ./cov.sh -m 74           # 抬高总覆盖阈值
#   ./cov.sh -p ./server     # 只跑一个包
#   ./cov.sh -r              # 复用已有 cover.out，不重跑测试
#   ./cov.sh --race          # 附带竞态检测（慢 3~5 倍）
#
# 需要数据库的测试依赖 ARTEX_PG_DSN；未设置时这些用例会自行跳过，
# 覆盖率会明显偏低 —— 脚本会在开头提醒。
set -euo pipefail

cd "$(dirname "$0")"

MIN="${ARTEX_COV_MIN:-65}"
PKGS="./..."
PROFILE="cover.out"
REUSE=0
RACE=0
TIMEOUT="1200s"
TOP="12"

usage() {
	sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
}

# --race 不占短选项，先摘出来再交给 getopts。
argv=()
for a in "$@"; do
	if [[ "$a" == "--race" ]]; then
		RACE=1
	else
		argv+=("$a")
	fi
done
set -- ${argv[@]+"${argv[@]}"}

while getopts ":m:p:o:t:rh" opt; do
	case "$opt" in
	m) MIN="$OPTARG" ;;
	p) PKGS="$OPTARG" ;;
	o) PROFILE="$OPTARG" ;;
	t) TOP="$OPTARG" ;;
	r) REUSE=1 ;;
	h) usage; exit 0 ;;
	\?) echo "未知参数：-$OPTARG" >&2; usage >&2; exit 2 ;;
	:) echo "-$OPTARG 需要一个值" >&2; exit 2 ;;
	esac
done

if [[ -z "${ARTEX_PG_DSN:-}" ]]; then
	echo "提示：ARTEX_PG_DSN 未设置，依赖数据库的用例会被跳过，覆盖率偏低。" >&2
fi

if ! command -v go >/dev/null 2>&1; then
	echo "找不到 go 命令" >&2
	exit 2
fi

args=(test "$PKGS" -count=1 -coverprofile="$PROFILE" -timeout "$TIMEOUT")
if [[ "$RACE" == "1" ]]; then
	args+=(-race)
fi

if [[ "$REUSE" == "0" ]]; then
	echo "> go ${args[*]}"
	if ! go "${args[@]}"; then
		echo "测试失败，覆盖率统计中止" >&2
		exit 1
	fi
elif [[ ! -f "$PROFILE" ]]; then
	echo "指定 -r 但 $PROFILE 不存在" >&2
	exit 2
fi

# 关键包的下限：总覆盖低是因为 cmd/selfupdate/guard 这些几乎无测试的包，
# 真正在维护的包不允许顺势下滑。要调就改这里，CI 复用本脚本，改一处即可。
FLOORS=(-pkg-min server=73 -pkg-min report=95 -pkg-min enrich=65 -pkg-min db=58)
SUMMARY=()
if [[ -n "${ARTEX_COV_SUMMARY:-}" ]]; then
	SUMMARY=(-summary "$ARTEX_COV_SUMMARY")
fi

echo
go run ./tools/covgate -profile "$PROFILE" -min "$MIN" -top "$TOP" \
	"${FLOORS[@]}" ${SUMMARY[@]+"${SUMMARY[@]}"}
