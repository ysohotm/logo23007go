# Cover pool partial rotate（全局汰旧补新）

日期：2026-11-28

## 目标

池目标仍是 `count`（如 10）。全局可设「每隔 N 天换 M 张」：删掉每个空背景池里最老的 M 张图，再排队补 M 张。例如 7 天换 5，或 1 天换 1。图片仍存硬盘；Mongo 只更新清单 count。

## 配置（全局）

存在现有 `coverpool_config` / `Settings`：

| 字段 | 含义 | 关闭 |
|---|---|---|
| `rotate_every_days` | 间隔天数 | `0` |
| `rotate_count` | 每轮每池删除并补回的张数 | `0` |

两者都 `>0` 才启用。`rotate_count` 钳制到 `1…count`。默认均为 `0`（行为与现网一致）。

## 运行时

- 进程内 `Rotator`：默认每小时一轮。
- 只处理 **空背景** 且当前张数 ≥ `count` 的池目录。
- 到期条件：`now - last_rotated_at >= N` 天；无 `last_rotated_at` 时用目录内最老图片 mtime。
- 按文件 mtime 升序删 M 张 → 写 `meta.json.last_rotated_at` → 刷新索引/Mongo → `Scheduler.Ensure` 补齐。
- 每轮最多处理 **50** 个池；若 `queue_len > queue_cap/2` 则本轮跳过。
- `POST /api/cover-pool/rotate` 手动跑同一逻辑（可选 `limit`）。

## 管理页

封面池配置表单增加两输入 +「立即轮换一轮」按钮；保存走现有 `PUT /api/cover-pool`。

## 非目标

- 按关键词单独节奏
- CDN 主动 purge
- 未满池（如 1/10）的补齐扫库（另案）
