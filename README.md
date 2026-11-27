# logo23007go

Go 移植版 [LogoMakerCore23007](../cursor/Logo23007/LogoMakerCore23007)：在背景图上绘制标题文字，输出封面图（Accept 协商 WebP / Progressive JPEG）与 PNG Logo，并用 MongoDB 做资源 ID 映射。

## 运行

```bash
export DBCONN='mongodb://...'
export DBNAME=cachelogomaker
# 可选
export PORT=23107
export CONTENT_ROOT=/app

go run .
```

将背景图放到 `app_data/images/`，字体已放在 `app_data/fonts/msyhbd.ttf`。

## 主要接口（与 C# 版对齐）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/vod/{text}` | 按标题生成封面（见下方格式协商） |
| GET | `/home/image?text=` | 同上（也兼容 `/Home/image` 等任意大小写） |
| GET | `/home/logo?text=` | 生成 PNG Logo |
| GET | `/home/imagekey?id=` | 按 filekey 生成图 |
| GET | `/home/addimagekey?domain=&key=` | 注册 filekey |
| POST | `/home/upload?filename=` | 上传背景图到 `app_data/images` |
| GET | `/home/fileisexits?fileName=` | 检查文件是否存在 |

封面接口按 `Accept` 协商输出格式：

- 请求头含 `image/webp` → `image/webp`（lossy quality 80）
- 否则 → Progressive JPEG（quality 80）
- 响应带 `Vary: Accept`，便于 CDN/浏览器按客户端缓存

`/Home/...` 与 `/home/...` 均可用：中间件会把 `/home` 与 action 段归一成小写后再路由。

## 关键词封面池与 Cloudflare 缓存

`/home/image`、`/vod/{text}` 与 `/home/imagekey` 仅对后台白名单尺寸做成品池：首次同步生成一张并返回，后台补齐到配置数量（默认 10，可设 1–100），之后随机读取。默认白名单为 `600x800`（未带尺寸后缀时的默认封面）；其它尺寸仍实时生成、不预生成。增大数量或新增尺寸后由后续访问补齐；减小数量或删尺寸不删除历史文件。补图共享正常生图并发限制；并发在后台「封面图片池」页设 1–32，队列默认 256。

成品保存在 `app_data/cache/covers/<哈希>/`。`COVER_POOL_DIR=off` 可关闭成品池；`COVER_POOL_QUEUE` 可调队列。`COVER_POOL_WORKERS` 已废弃（忽略），并发以后台配置为准。

成功封面响应带：

```text
Cache-Control: public, max-age=86400, s-maxage=2592000
Cloudflare-CDN-Cache-Control: public, max-age=2592000
Vary: Accept
```

浏览器缓存 1 天，Cloudflare 边缘缓存 30 天。CF 命中期间固定返回已缓存图片，回源后才重新随机。确保域名对应缓存规则允许缓存这些图片路径，并尊重源站的上述缓存头。

`vod` 文本支持 `_240x300` 等尺寸后缀、`_orig`、末尾 `_{数字ID}` / `_{32位md5}` 绑定背景。未指定尺寸时默认 600×800；请求 1200×1520 会被强制降为 600×800。

## 访问限制（ipguard）与后台

所有业务请求先过 `internal/ipguard` 中间件，按顺序命中即放行，否则 `403`：

1. 真实 IP 命中 IP/CIDR 白名单（默认：bwh8 / dmit01 / dmit02 / hehk03 / v99 + 回环/内网段）
2. 真实 IP 命中 IP/CIDR 黑名单 → 直接 `403`（优先于 UA 白名单与国家放行）
3. User-Agent 包含 UA 白名单关键字（默认常见搜索引擎蜘蛛）
4. `CF-IPCountry` 在放行国家（默认 `HK, CN, SG`）；无该头时按开关决定（默认放行）

真实 IP 由 `internal/clientip` 解析：仅当连接对端是可信代理（Cloudflare 段、自有服务器、内网）时才读取
`CF-Connecting-IP` / `X-Forwarded-For` / `X-Real-IP` 和 `CF-IPCountry`，直连伪造的头会被忽略。

后台：`/ytmadmin/`（无默认账号，首次访问创建管理员），可在线修改上述规则、查看拦截统计与最近 500 条拦截记录；
「访问分析」页展示最近 10 分钟每分钟请求数（放行 / 403 / 其中黑名单 / 503），以及最近 1 万条访问按网段（IPv4 /24、IPv6 /48）
聚合的请求数、拦截数、IP 数、UA 数和国家，可单个或批量一键加入黑名单（均为进程内存，重启清空）；
「账号」页可修改用户名/密码（需当前密码，修改后旧会话全部失效）。
忘记密码时删除 Mongo 中的管理员文档后重新访问即可重建：`db.admin_user.deleteOne({_id:"admin"}); db.admin_sessions.deleteMany({})`。
规则存 Mongo 集合 `ipguard_config`，保存即生效，各实例每 30 秒同步一次；`/ytmadmin`、`/api/*` 不受限制（API 需登录）。

## Docker

镜像已发布到 GHCR。**把整个 `app_data` 挂到宿主机即可**，关键词成品池和背景缓存都在其下，不必再单独挂 `cache`：

| 容器路径 | 默认宿主机（v99） | 说明 |
|---|---|---|
| `/app/app_data` | `/data/logomaker23007` | 字体、背景图、全部缓存 |
| `…/cache/covers` | `…/cache/covers` | 关键词成品池（自动创建） |
| `…/cache/bg` | `…/cache/bg` | 背景缩放缓存（自动创建） |

```bash
# 预建目录（首次）
mkdir -p /data/logomaker23007/{fonts,images,cache/covers,cache/bg}

# 拉取并运行（host 网络 + 东八区）
docker pull ghcr.io/ysohotm/logo23007/logo23007go:latest
docker run -d --name logo23107 --network host --restart always \
  -e DBCONN='mongodb://...' \
  -e DBNAME=logo23107 \
  -e TZ=Asia/Shanghai \
  -e PORT=23107 \
  -v /usr/share/zoneinfo/Asia/Shanghai:/etc/localtime:ro \
  -v /data/logomaker23007:/app/app_data \
  ghcr.io/ysohotm/logo23007/logo23007go:latest

# v99 一键滚动（继承旧容器 DBCONN、打回退 tag、预建 cache）
# bash deploy/run-v99.sh
# bash deploy/run-v99.sh 2026.1126.1010

# 重新构建并推送
./pubdocker.sh ghcr
```

可选环境变量：`COVER_POOL_DIR`（默认 `app_data/cache/covers`，`off` 关闭）、`COVER_POOL_QUEUE`、`BG_CACHE_DIR`。

包地址：https://github.com/users/ysohotm/packages/container/package/logo23007%2Flogo23007go  
（当前为 private，需登录 ghcr 后拉取）
