# 🌱 每日任务模块（daily/）

WorkBuddy 成长中心 / 开学季 / 小程序等每日任务的自动化组件，供 `web/` 管理面板的「每日任务」页与系统 cron 调用。

> ⚠️ 任务脚本来自第三方开源项目（L0NE-6/WorkBuddy-Daily），涉及平台自动化操作，请自行评估并承担合规风险。

## 目录内容

| 文件 | 说明 |
|---|---|
| `run_daily.py` | 执行器（本仓库维护）：从网关 auths 重建 AT → 调用任务脚本 → 解析输出写 `status.json` / `logs/` |
| `workbuddy_daily.py` | 任务脚本：上游 [L0NE-6/WorkBuddy-Daily](https://github.com/L0NE-6/WorkBuddy-Daily) @ `9c531f9`，MIT，原样引入 |
| `workbuddy_login.py` | 上游配套的登录 / 取 token 工具（同版本） |
| `requirements.txt` | Python 依赖（仅 `requests`，Python 3.8+） |
| `LICENSE.workbuddy-daily` | 上游 MIT 许可（版权归 L0NE-6，按许可要求保留） |
| `sync-upstream.sh` | 上游更新检查 / 应用脚本 |

## 设计约束（AT-only）

- RT（refresh token）轮换所有权归 workbuddy2api 网关（默认每日 22:00 keepalive）。
- 执行器只从 `AUTH_DIR`（默认 `/opt/workbuddy2api/auths`，可用环境变量 `DAILY_AUTH_DIR` 覆盖）读取 `workbuddy-*.json` 提取 accessToken，重建 `WORKBUDDY_ACCESS_TOKEN.txt`；
- 绝不设置 `WORKBUDDY_REFRESH_TOKEN`、绝不生成 `wb_refresh_tokens.json`，保证任务脚本 `auto_refresh()` 短路、永不轮换 RT。
- 执行器自带文件锁（`run.lock`），重复 / 并发触发会安全跳过。

## 部署（三步）

```bash
# 1) 放置脚本（示例：/opt/workbuddy-daily）
mkdir -p /opt/workbuddy-daily
cp daily/run_daily.py daily/workbuddy_daily.py /opt/workbuddy-daily/

# 2) 安装依赖（仅 requests）
pip3 install -r daily/requirements.txt

# 3) 定时任务（示例 /etc/cron.d/workbuddy-daily，本地时间）
#    夜猫子任务仅 23:00–08:00 窗口计入进度，故需要 23:30 这一班
30 7  * * * root cd /opt/workbuddy-daily && /usr/bin/python3 run_daily.py --mode morning >> logs/cron.log 2>&1
30 23 * * * root cd /opt/workbuddy-daily && /usr/bin/python3 run_daily.py --mode night   >> logs/cron.log 2>&1
```

## 与面板联调

`web/` 管理面板通过 `DAILY_DIR`（默认 `/opt/workbuddy-daily`）读取 `status.json` 与 `logs/run.log`；保持两者指向同一目录，即可在面板中查看执行状态、实时日志并手动触发。

## 手动运行

```bash
python3 run_daily.py --mode manual             # 全账号
python3 run_daily.py --mode manual --only 1    # 仅第 1 个账号；其余参数透传给任务脚本
```

## 上游同步

```bash
./sync-upstream.sh           # 检查上游是否有更新（dry-run）
./sync-upstream.sh --apply   # 应用更新（自动备份旧文件；建议替换后先试跑再上生产）
```

> 上游项目仍在活跃更新；本仓库按已验证版本 `9c531f9` 固定引入。升级后请先在测试目录验证，再替换生产文件。
