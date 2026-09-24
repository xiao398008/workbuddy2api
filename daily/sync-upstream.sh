#!/usr/bin/env bash
# 上游 WorkBuddy-Daily（L0NE-6）同步辅助脚本
# 用法：
#   ./sync-upstream.sh           # dry-run：检查上游是否有更新并展示差异概览
#   ./sync-upstream.sh --apply   # 下载并替换 workbuddy_daily.py（自动备份旧文件）
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPSTREAM_RAW="https://raw.githubusercontent.com/L0NE-6/WorkBuddy-Daily/main"
TARGET="$DIR/workbuddy_daily.py"

TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

echo "== 拉取上游 main: workbuddy_daily.py =="
curl -fsSL "$UPSTREAM_RAW/workbuddy_daily.py" -o "$TMP"

old_md5="$(md5sum "$TARGET" | awk '{print $1}')"
new_md5="$(md5sum "$TMP" | awk '{print $1}')"
echo "本地: $old_md5"
echo "上游: $new_md5"

if [ "$old_md5" = "$new_md5" ]; then
  echo "已是最新（与上游 main 一致），无需处理。"
  exit 0
fi

echo
echo "== 上游有更新，变更概览 =="
diff "$TARGET" "$TMP" | awk '/^</{d++} /^>/{a++} END{printf "  - 删除/修改行: %d\n  + 新增行: %d\n", d+0, a+0}' || true

if [ "${1:-}" = "--apply" ]; then
  bak="$TARGET.bak-$(date +%Y%m%d-%H%M%S)"
  cp "$TARGET" "$bak"
  cp "$TMP" "$TARGET"
  echo
  echo "已替换（备份：$bak）。"
  echo "建议先在测试环境试跑：python3 run_daily.py --mode manual --only 1"
else
  echo
  echo "当前为 dry-run。确认后执行：./sync-upstream.sh --apply"
fi
