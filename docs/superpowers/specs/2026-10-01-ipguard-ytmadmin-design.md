# 访客 IP 限制 + /ytmadmin 后台 设计

## 目标

对所有业务请求做来源限制，并提供 `/ytmadmin/` 后台在线调整规则。

## 真实 IP（`internal/clientip`）

- 仅当连接对端（`RemoteAddr`）是可信代理时才读取 `CF-Connecting-IP` → `X-Forwarded-For`（从右往左第一个非可信）→ `X-Real-IP`。
- 可信代理 = 回环/内网段 + Cloudflare 官方段 + 自有服务器：
  bwh8 `80.251.212.10`（tailscale `100.95.91.5`）、dmit01 `179.253.236.72`、dmit02 `179.255.115.142`、
  hehk03 `207.56.22.48`、v99 `104.238.220.50`。
- `FromTrustedPeer(r)` 供上层判断是否可信任 `CF-IPCountry`。

## 放行规则（`internal/ipguard`）

按顺序，命中即放行：

1. 规则关闭 → 放行
2. 真实 IP 命中 IP/CIDR 白名单（默认：5 台服务器 + 回环 + 内网段）
3. UA 包含白名单关键字（不区分大小写；默认常见搜索引擎蜘蛛）
4. `CF-IPCountry`（仅对端可信时读取，防伪造）：
   - 为空 → 按「缺少国家头时放行」开关（默认放行）
   - 在国家列表（默认 `HK, CN, SG`）→ 放行
5. 其余 → `403`

`/ytmadmin`、`/api/*` 豁免（后台 API 仍需登录）。

## 存储（MongoDB，复用 `DBNAME`）

| 集合 | 用途 |
|------|------|
| `ipguard_config` | 单文档 `_id=default`：enabled / countries / allow_no_country / ips / uas |
| `admin_user` | 单管理员 `_id=admin`，bcrypt 密码 |
| `admin_sessions` | `_id=sha256(token)`，`expires_at` TTL 索引，有效期 7 天 |

配置保存即生效；每 30s 从 Mongo 重载，多实例同步；Mongo 不可用时保留上次配置（首次为默认值）。

## 后台

- `index.html`：首次创建管理员 / 登录
- `ipguard.html`：开关、国家、IP 白名单、UA 白名单；「我的访问检测」
- `blocked.html`：放行/拦截计数、按国家拦截统计、最近 500 条拦截记录（内存）

## 测试

- `clientip`：可信/不可信对端、XFF 右到左、伪造头忽略
- `ipguard`：各放行分支、伪造 `CF-IPCountry` 无效、配置校验、豁免路径
- `admin`：用假 AuthStore 测 setup/login/鉴权、配置读写
