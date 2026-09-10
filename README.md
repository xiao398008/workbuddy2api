# WorkBuddy2API (含专业管理面板 & 动态计费看板)

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white&style=flat-square)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-green.svg?style=flat-square)](LICENSE)
[![OpenAI Compatible](https://img.shields.io/badge/API-OpenAI%20Compatible-412991?style=flat-square)](https://platform.openai.com)

> 将腾讯 CodeBuddy 账号池转换为标准 OpenAI 兼容 `/v1/chat/completions` API 的高性能多账号反向网关，自带现代化 Web 管理与数据监控面板。

---

## ✨ 核心特性

- **OpenAI 格式完全兼容**：支持流式（SSE）与非流式调用，兼容各主流客户端与第三方框架。
- **现代化可视化面板 (Web Dashboard)**：
  - **实时请求流水监控**：支持查看单次请求的 TTFB、Token 生成速率、选号 UID、上游状态码。
  - **全维度 Token 与积分审计**：精确统计每日输入、输出、Prompt 缓存命中数 (Prompt Cache Hit) 及上游实际扣减积分 (`cr`)。
  - **动态模型与实时倍率**：展示上游实时计费倍率与官方促销标签（Badge 折扣），支持手动极速刷新（带上游防抖节流保护）。
  - **账号池全景与配额管理**：实时查看各账号当前剩余 credits、已用额度、套餐过期时间、自动签到与保活状态。
  - **便捷 OAuth 登录**：内置扫码/授权登录辅助，自动捕获 Access Token 并写入账号池。
- **智能账号调度与熔断保护**：
  - **三因子加权负载**：结合积分余量、空闲时长补偿、调用成功率进行账号轮换。
  - **自适应熔断机制**：连续失败自动移入冷却期，防止坏号拖垮整体集群服务。
  - **会话粘性 (Session Stickiness)**：支持基于会话键的多轮对话绑定，保障上下文稳定性。
- **全链路严密脱敏与防御**：自动清洗指纹与追踪头，遵循官方客户端请求规范。

---

## 🚀 快速开始

### 1. 环境准备
- Go 1.22+ 或 Docker

### 2. 配置文件说明
复制样例配置并设置您的网关鉴权密钥：
```bash
cp config.example.json config.json
```
根据需要修改 `config.json` 中的 `api_key`。

### 3. 本地编译与运行

#### 启动核心网关服务：
```bash
go build -o wb2api cmd/server/main.go
./wb2api
```
网关默认监听在 `:7863` 端口。

#### 启动 Web 管理面板：
```bash
cd web
go build -o workbuddy-web main.go
ADMIN_KEY="your-admin-password" ./workbuddy-web
```
管理面板默认监听在 `:7864` 端口，打开浏览器访问 `http://localhost:7864` 即可进入管理看板。

---

## 🛠️ Docker 部署

```bash
docker compose up -d
```

---

## 🔒 脱敏与开源安全声明
本项目所有涉及凭证、网关 Key、第三方 Token 及测试账号 UID 均已完成严格脱敏，不含任何私有硬编码数据。生产部署请务必修改默认口令。
