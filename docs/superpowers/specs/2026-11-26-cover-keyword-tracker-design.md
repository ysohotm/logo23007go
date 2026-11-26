# 手工关键词进度跟踪设计

## 目标

后台手工添加的关键词写入 Mongo 集合，按「词 × 尺寸」展示已生成数量与目标数量，方便补齐到配置值。出图热路径仍走本地封面池随机读取，不查该集合。

## 数据

集合 `cover_keywords`，一词一文档：

```json
{
  "_id": "规范化关键词",
  "text": "规范化关键词",
  "created_at": "...",
  "updated_at": "..."
}
```

列表接口实时用磁盘池 `Pool.Count(spec)` + 当前全局 `count`/`sizes` 拼出各尺寸进度；不在文档里缓存易脏的 count 字段（避免与磁盘不一致）。文档只负责「登记过哪些词」。

## API

- `GET /api/cover-pool/keywords`：返回登记关键词及每尺寸 `{w,h,count,target}`（需登录）
- `POST /api/cover-pool/generate`：入队补齐后 upsert 关键词到集合（已有行为扩展）

## 后台

`covers.html` 增加关键词进度表：每行一词，各白名单尺寸显示 `count/target`；支持刷新；未满可再次生成补齐。

## 验收

1. 手工生成后集合有文档；列表显示各尺寸进度。
2. 磁盘池补齐后刷新列表 count 上升。
3. 出图路径不依赖该集合。
4. `go test ./...` 通过；部署 ss81 后后台可用。
