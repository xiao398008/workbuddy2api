#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""WorkBuddy-Daily 任务执行器（AT-only 模式）。

设计约束：
  · RT（refresh token）轮换所有权归 workbuddy2api 网关（每日 22:00 keepalive）。
  · 本执行器只从 AUTH_DIR（默认 /opt/workbuddy2api/auths，可用 DAILY_AUTH_DIR 覆盖）下的 *.json 提取 AT 重建 WORKBUDDY_ACCESS_TOKEN.txt，
    绝不设置 WORKBUDDY_REFRESH_TOKEN、绝不生成 wb_refresh_tokens.json，
    以保证 workbuddy_daily.py 的 auto_refresh() 直接短路、永不轮换 RT。
  · 每次运行结果写入 status.json（供前端展示）+ logs/run.log（实时日志）。

用法：
  python3 run_daily.py --mode morning          # 全流程（cron 07:30）
  python3 run_daily.py --mode night            # 全流程（cron 23:30，夜猫子窗口）
  python3 run_daily.py --mode manual --only 1  # 手动；未知参数透传给 workbuddy_daily.py
"""
import fcntl
import glob
import json
import os
import re
import shutil
import subprocess
import sys
import threading
import time
from datetime import datetime, timedelta, timezone

DIR = os.path.dirname(os.path.abspath(__file__))
AUTH_DIR = os.environ.get("DAILY_AUTH_DIR", "/opt/workbuddy2api/auths")
SCRIPT = os.path.join(DIR, "workbuddy_daily.py")
TOKEN_FILE = os.path.join(DIR, "WORKBUDDY_ACCESS_TOKEN.txt")
REFRESH_STORE = os.path.join(DIR, "wb_refresh_tokens.json")
STATUS_FILE = os.path.join(DIR, "status.json")
LOG_DIR = os.path.join(DIR, "logs")
LOG_FILE = os.path.join(LOG_DIR, "run.log")
ARCHIVE_DIR = os.path.join(LOG_DIR, "archive")
LOCK_FILE = os.path.join(DIR, "run.lock")
MAX_SECONDS = 7200
KEEP_ARCHIVES = 30

CST = timezone(timedelta(hours=8))


def now_str():
    return datetime.now(CST).strftime("%Y-%m-%d %H:%M:%S")


def log(msg):
    line = "[%s] %s" % (now_str(), msg)
    print(line, flush=True)
    try:
        os.makedirs(LOG_DIR, exist_ok=True)
        with open(LOG_FILE, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except Exception:
        pass


def write_status(data):
    tmp = STATUS_FILE + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=1)
    os.replace(tmp, STATUS_FILE)


def read_status():
    try:
        with open(STATUS_FILE, encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return None


def pid_alive(pid):
    try:
        os.kill(int(pid), 0)
        return True
    except Exception:
        return False


def rebuild_token_file():
    """从 wb2api auths 重建 AT 文件（@ 分隔），返回 [(uid, at), ...]。"""
    entries = []
    for f in sorted(glob.glob(os.path.join(AUTH_DIR, "workbuddy-*.json"))):
        try:
            with open(f, encoding="utf-8") as fh:
                d = json.load(fh)
        except Exception as e:
            log("读取凭据失败 %s: %s" % (os.path.basename(f), e))
            continue
        uid = (d.get("account") or {}).get("uid", "")
        at = (d.get("auth") or {}).get("accessToken", "")
        if at:
            entries.append((uid, at))
    if not entries:
        raise RuntimeError("auths 目录中没有可用 accessToken")
    with open(TOKEN_FILE, "w", encoding="utf-8") as f:
        f.write("@".join(at for _, at in entries))
    os.chmod(TOKEN_FILE, 0o600)
    log("已从 auths 重建 AT 文件：%d 个账号" % len(entries))
    return entries


TAG_RE = re.compile(r"^\[(\d{2}:\d{2}:\d{2})\]\[(账号\d+)\]\s?(.*)$")
FIN_IN_BODY = re.compile(r"^🏁\s*(账号\d+): 完成(\d+)/(\d+) 等级(\S+) 剩余: (.*)$")


def parse_output(lines, uid_map, idx_map):
    """把脚本输出解析为每账号结果。uid_map/idx_map: {'账号1': uid/全局序号, ...}

    脚本所有输出都带 [HH:MM:SS][账号N] 前缀（含 🏁 结算行），逐行剥壳后再匹配。
    """
    accs = {}
    for line in lines:
        m = TAG_RE.match(line)
        if not m:
            continue
        tag, body = m.group(2), m.group(3)
        ent = accs.setdefault(tag, {})
        if body.startswith("💰 积分:"):
            ent["credits"] = body[len("💰 积分:"):].strip()
        elif body.startswith("📊 用量:"):
            ent["usage"] = body[len("📊 用量:"):].strip()
        elif body.startswith("🌱 成长:"):
            ent["growth"] = body[len("🌱 成长:"):].strip()
        fin = FIN_IN_BODY.match(body)
        if fin:
            ent["done"] = int(fin.group(2))
            ent["total"] = int(fin.group(3))
            ent["level"] = fin.group(4)
            rest = fin.group(5).strip()
            ent["rest"] = [] if rest in ("", "无") else [x.strip() for x in rest.split(",") if x.strip()]
    out = []
    for tag in sorted(accs, key=lambda t: idx_map.get(t, 999)):
        ent = accs[tag]
        ent["note"] = tag
        ent["uid"] = uid_map.get(tag, "")
        ent["idx"] = idx_map.get(tag, 0)
        ent.setdefault("rest", ["（未完成解析，详见日志）"])
        out.append(ent)
    return out


def build_maps(entries, argv):
    """构建 标签→uid / 标签→全局序号 映射。

    注意：脚本带 --only N 时会把过滤后的账号重编号为「账号1」，因此 --only 场景下
    标签“账号1”应对应 entries[N-1]，序号也为 N。
    """
    only = None
    if "--only" in argv:
        try:
            only = int(argv[argv.index("--only") + 1])
        except Exception:
            only = None
    if only and 0 < only <= len(entries):
        return {"账号1": entries[only - 1][0]}, {"账号1": only}
    uid_map = {"账号%d" % (i + 1): uid for i, (uid, _) in enumerate(entries)}
    idx_map = {"账号%d" % (i + 1): i + 1 for i in range(len(entries))}
    return uid_map, idx_map


def prune_archives():
    try:
        files = sorted(glob.glob(os.path.join(ARCHIVE_DIR, "run-*.log")))
        for f in files[:-KEEP_ARCHIVES]:
            os.remove(f)
    except Exception:
        pass


def tee_run(args, env):
    """运行 workbuddy_daily.py，输出同时进日志与内存。返回 (rc, lines, duration)。"""
    buf = []
    t0 = time.time()
    os.makedirs(ARCHIVE_DIR, exist_ok=True)
    with open(LOG_FILE, "a", encoding="utf-8") as f:
        f.write("═══ %s 启动: python3 %s %s ═══\n" % (now_str(), SCRIPT, " ".join(args)))
        proc = subprocess.Popen(
            [sys.executable, "-u", SCRIPT] + args,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, bufsize=1, cwd=DIR, env=env,
        )
        killer = threading.Timer(MAX_SECONDS, proc.kill)
        killer.start()
        try:
            for line in proc.stdout:
                f.write(line)
                f.flush()
                buf.append(line.rstrip("\n"))
        finally:
            killer.cancel()
        rc = proc.wait()
        dur = time.time() - t0
        f.write("═══ %s 结束 rc=%d 耗时 %.0fs ═══\n" % (now_str(), rc, dur))
    try:
        shutil.copy(LOG_FILE, os.path.join(ARCHIVE_DIR, "run-%s.log" % datetime.now(CST).strftime("%Y%m%d-%H%M%S")))
        prune_archives()
    except Exception:
        pass
    return rc, buf, dur


def main():
    argv = sys.argv[1:]
    mode = "manual"
    if "--mode" in argv:
        i = argv.index("--mode")
        mode = argv[i + 1] if i + 1 < len(argv) else "manual"
        del argv[i:i + 2]

    os.makedirs(LOG_DIR, exist_ok=True)
    lock_fh = open(LOCK_FILE, "w")
    try:
        fcntl.flock(lock_fh, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        log("已有任务在运行（lock 占用），本次跳过")
        return 3

    open(LOG_FILE, "w").close()  # 新一轮运行截断实时日志（历史保留在 logs/archive/）
    started = now_str()
    write_status({"running": True, "mode": mode, "pid": os.getpid(),
                  "started_at": started, "finished_at": None, "exit_code": None,
                  "duration_sec": None, "log_file": "logs/run.log",
                  "accounts": [], "tail": ""})

    # 双保险：确保脚本处于 AT-only 模式
    if os.path.exists(REFRESH_STORE):
        os.remove(REFRESH_STORE)
        log("已移除 wb_refresh_tokens.json（AT-only 模式约束）")
    env = os.environ.copy()
    env.pop("WORKBUDDY_REFRESH_TOKEN", None)
    env["PYTHONUNBUFFERED"] = "1"
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env["TZ"] = "Asia/Shanghai"
    env["NO_COLOR"] = "1"

    try:
        entries = rebuild_token_file()
    except Exception as e:
        log("重建 AT 失败: %s" % e)
        st = read_status() or {}
        st.update({"running": False, "finished_at": now_str(), "exit_code": 1,
                   "error": "重建 AT 失败: %s" % e})
        write_status(st)
        return 1

    uid_map, idx_map = build_maps(entries, argv)

    log("开始执行（mode=%s args=%s 账号数=%d）" % (mode, " ".join(argv) or "-", len(entries)))
    rc, lines, dur = tee_run(argv, env)
    accounts = parse_output(lines, uid_map, idx_map)
    log("执行结束 rc=%d 耗时=%.0fs 账号结果=%d" % (rc, dur, len(accounts)))

    st = {
        "running": False,
        "mode": mode,
        "pid": os.getpid(),
        "started_at": started,
        "finished_at": now_str(),
        "exit_code": rc,
        "duration_sec": int(dur),
        "log_file": "logs/run.log",
        "accounts_total": len(entries),
        "accounts": accounts,
        "tail": "\n".join(lines[-120:]),
    }
    write_status(st)
    return rc


if __name__ == "__main__":
    sys.exit(main())
