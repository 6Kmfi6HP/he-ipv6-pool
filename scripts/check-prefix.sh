#!/bin/sh
# 检查某个 routed 前缀是否已经可用：用前缀内的地址经 he-ipv6-pool 访问 api64.ipify.org，
# 返回的出口地址等于该地址即视为可用（去程和回程都通）。适合在 HE 新分配前缀后定时运行。
#
# 用法：check-prefix.sh <前缀内的测试地址>    例如 check-prefix.sh 2001:db8:abcd::1
# 地址必须写成压缩的规范形式（与 api64.ipify.org 返回的格式一致），并且所在前缀已写进配置的 prefixes。
# 可用环境变量覆盖：BIN、CONF、SOCKS、PROXY_USER、PROXY_PASS、LOG。
# 退出码：0 可用，1 不可用。he-ipv6-pool 没有在运行时会临时启动，检查完再停掉。
set -u
ADDR=${1:?用法: $0 <前缀内的测试地址>}
DIR=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-$DIR/he-ipv6-pool}
CONF=${CONF:-$DIR/config.json}
SOCKS=${SOCKS:-127.0.0.1:1080}
PROXY_USER=${PROXY_USER:-check}
PROXY_PASS=${PROXY_PASS:-x}
LOG=${LOG:-$DIR/check-prefix.log}

started=""
if ! pgrep -x he-ipv6-pool >/dev/null; then
	"$BIN" -c "$CONF" >/dev/null 2>&1 &
	started=$!
	sleep 2
fi
user="$PROXY_USER-ip-$(echo "$ADDR" | tr ':' '-')"
got=$(curl -s -m 20 -x "socks5h://$user:$PROXY_PASS@$SOCKS" https://api64.ipify.org)
[ -n "$started" ] && kill "$started"

ts=$(date -u '+%F %T UTC')
if [ "$got" = "$ADDR" ]; then
	echo "$ts READY $ADDR" | tee -a "$LOG"
	exit 0
fi
echo "$ts NOT-READY $ADDR (出口: ${got:-无响应})" | tee -a "$LOG"
exit 1
