# 本地元数据目录服务（Metadata Lineage Catalog）

纯 Go 标准库实现的本地元数据目录，先管两块：

- **数据资产与 schema 版本登记**：稳定业务键定位、幂等上报、schema 兼容性判定、历史版本不可变、任意版本差异对比；
- **字段级血缘**：边精确到「资产 + 字段」，可登记/可撤销，支持下游影响面查询与具体传播路径解释。

不接任何外部系统：状态落本地单个文件，HTTP 提供本地入口，方便手工 `curl` 验证。

## 启动

```bash
# 默认监听 127.0.0.1:8080，状态文件 ./data/catalog-state.json
go run ./cmd/catalog

# 自定义
go run ./cmd/catalog -addr :18080 -data /tmp/catalog.json
# 或环境变量
CATALOG_ADDR=:18080 CATALOG_DATA=/tmp/catalog.json go run ./cmd/catalog
```

健康检查：`GET /healthz` → `{"status":"ok"}`。

## 数据模型

- `Asset`：`id`（内部生成）、`key`（稳定业务键，唯一）、`name`/`kind`（可变元数据）、
  `create_time`/`update_time`、`deleted`/`delete_time`（软删除标记）。
- `SchemaVersion`：从 1 开始、严格连续递增；`columns` 为不可变快照，
  `content_hash` 为「资产 + 版本号 + 列集合」的 SHA-256，用于识别内容变化与落盘校验。
- `Column`：`name`、`type`、`nullable`、`doc`。列顺序属于版本内容的一部分（顺序不同即不同内容）。
- `Edge`：`src_asset_key/src_column → dst_asset_key/dst_column`，表示「源字段产出目的字段」。

### 业务键与幂等

- 同一 `key` 重复登记资产：元数据（name/kind）相同时完全幂等；不同时只更新可变元数据，不产生 schema 版本。
- 同一资产重复上报**与最新版本列集合完全相同**的 schema：直接返回已有版本（HTTP 200，`created=false`），不新增版本号。
- 版本一旦写入永不修改：任何「同版本号不同内容」的写法都会被拒绝，历史版本始终按版本号可查。
- 血缘边重复登记幂等（`created=false`）；撤销不存在的边返回 `deleted=false` 且不报错——撤销操作的目标状态确定。

## Schema 兼容判定规则

新版本相对**最新版本**判定（`POST /assets/{key}/versions`）：

兼容、直接放行：

- **新增列**；
- **类型宽化**：有符号整型按 `int8 < int16 < int32 < int64` 同链宽化；
  `float32 → float64`；任意数值类型 → `decimal`；数值/`bool` → `string`；
- **放宽可空**：`NOT NULL → NULL`。

破坏性变化，拒绝（HTTP 422 `incompatible_schema`），响应里 `incompatible_changes`
逐项给出 `kind`、涉及列、旧值/新值与人类可读原因：

- `dropped_column`：删列（按名引用该列的下游会断）；
- `narrowed_type`：收窄类型（如 `int64 → int32`）或跨族转换（`int → float`、`string → 数值` 等）；
- `nullability_tightened`：`NULL → NOT NULL`（存量 NULL 行无法写入）；
- `renamed_column`：改名。识别启发式——被删列与新增列类型（含可空性）完全相同且配对唯一时判定为改名，
  因为按列名建立的血缘会随之失效。

注意：破坏性变更被拒绝后不会产生新版本，历史版本保持不变。

### 版本差异

`GET /assets/{key}/diff?from=1&to=3` 返回两个版本间 `added` / `removed` / `changed`
（同名列的类型、可空性变化），`from`/`to` 可任意指定（支持反向对比）。

## 血缘语义与影响面

- 边端点必须解析到**存在且未删除**的资产，字段必须存在于该资产**最新版本** schema。
  失败类型严格区分，调用方不会拿到「随便挑一个」的结果：
  - `ambiguous_key`（HTTP 409）：业务键有歧义（例如损坏数据引入的历史别名指向多个资产）；
  - `not_found`（HTTP 404）：资产不存在；
  - `dangling_reference`（HTTP 422）：资产存在但字段不存在（悬空字段），`details` 列出每个悬空端点。
  - `validation_error`（HTTP 400）：自环边（源和目的是同一字段）或参数缺失。
- **允许成环**（`a→b→c→a`），仅禁止自环；影响面遍历用访问集合去重，环不会导致挂死或重复计数。
- **影响面**（`GET /impact?asset_key=&column=`）：沿 `src → dst` 做 BFS，
  返回除源字段外全部可达字段，按 `asset_key,column` 字典序排列——
  **结果与边的登记顺序无关**；每个受影响字段附 `path`（一条最短路径）解释影响如何传播。
  新登记/撤销的边随下一次请求立即生效。BFS 复杂度 O(V+E)，资产数千、边上万时单次查询在秒级以内。
- 撤销：`DELETE /edges`（JSON body 或 query 参数指定四元组），幂等。

### 删除与历史可追溯

`DELETE /assets/{key}` 为**软删除**：之后不能再登记新版本或新增边（返回 `conflict`），
但资产本身、全部历史版本、历史血缘仍可查询（`GET /assets?include_deleted=true`、
`GET /assets/{key}/versions`、`GET /edges` 都保留）。删除本身幂等。

## 并发一致性

- 所有可变状态保存在单个不可变 `snapshot` 指针背后；写操作在互斥锁内 copy-on-write
  构造新快照并整体替换，读操作取到快照指针后即可在锁外遍历——**读到的必然是某一次完整提交**，
  不会看到写了一半的资产、版本或血缘。
- 失败重试/重复操作不产生重复版本、重复边：版本按「内容相同即同一版本」判重，边按四元组指纹判重。
- 写持久化失败时内存状态回滚到磁盘状态，避免「内存新、磁盘旧」诱发重试产生重复数据。

## 本地落盘说明

- 状态文件（默认 `./data/catalog-state.json`）是带校验和的信封：
  `{"schema_version":1,"sha256":"<data 的 SHA-256>","data":{...}}`。
- **原子写**：写同目录临时文件 → `fsync` → `rename` 原子替换 → `fsync` 目录。
  进程在任意时刻被杀，重启后要么是上一份完整文件、要么是新的完整文件，不会出现半写状态。
- 启动加载时按类型识别损坏，绝不把坏数据当正常数据返回：
  - JSON 截断/语法错误、空文件 → `corrupt_store`；
  - 校验和不匹配（被部分覆盖）→ `corrupt_store`；
  - 信封版本不认识 → `unsupported_store_version`；
  - 语义结构损坏（悬空索引、重复版本号、版本不连续、内容 hash 不符、边引用缺失字段等）
    → `corrupt_store`，`details` 逐项列出问题。
- 兼容范围：仅支持信封 `schema_version=1`；字段集以前向演进为原则（未知 JSON 字段会被拒绝）。
  当前定位为单机本地工具，不支持多进程同时写同一状态文件。

## HTTP 接口一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查 |
| POST | `/assets` | 登记/更新资产 `{key,name,kind}` |
| GET | `/assets?include_deleted=true` | 列出资产（按 key 排序） |
| GET | `/assets/{key}` | 资产详情 + 全部版本 |
| DELETE | `/assets/{key}` | 软删除（幂等） |
| POST | `/assets/{key}/versions` | 登记下一版 schema `{columns:[...]}` |
| GET | `/assets/{key}/versions` | 版本列表 |
| GET | `/assets/{key}/versions/{v}` | 指定版本 |
| GET | `/assets/{key}/diff?from=&to=` | 两版本差异 |
| POST | `/edges` | 登记血缘边 |
| DELETE | `/edges` | 撤销血缘边（幂等） |
| GET | `/edges` | 列出全部边（顺序稳定） |
| GET | `/impact?asset_key=&column=` | 字段下游影响面 + 路径 |

## 手工验证示例

```bash
curl -s localhost:8080/assets -d '{"key":"db.orders","name":"订单表"}'
curl -s localhost:8080/assets/db.orders/versions -d '{
  "columns":[{"name":"id","type":"int32","nullable":false},
             {"name":"amt","type":"int64","nullable":true}]}'
# 兼容演进：int32->int64 宽化 + 新增 ts
curl -s localhost:8080/assets/db.orders/versions -d '{
  "columns":[{"name":"id","type":"int64","nullable":false},
             {"name":"amt","type":"int64","nullable":true},
             {"name":"ts","type":"timestamp","nullable":true}]}'
# 破坏性演进（收窄+删列）→ 422，逐项列出 incompatible_changes
curl -s -i localhost:8080/assets/db.orders/versions -d '{
  "columns":[{"name":"id","type":"int32","nullable":false}]}'

curl -s 'localhost:8080/assets/db.orders/diff?from=1&to=2'

curl -s localhost:8080/assets -d '{"key":"dw.dwd_orders"}'
curl -s localhost:8080/assets/dw.dwd_orders/versions -d '{
  "columns":[{"name":"order_id","type":"int64","nullable":false}]}'
curl -s localhost:8080/edges -d '{
  "src_asset_key":"db.orders","src_column":"id",
  "dst_asset_key":"dw.dwd_orders","dst_column":"order_id"}'
curl -s 'localhost:8080/impact?asset_key=db.orders&column=id'
```

## 测试

```bash
go test ./...                 # 全量（含数千资产/上万边规模用例）
go test -short ./...          # 跳过规模用例
CGO_ENABLED=1 go test -race ./...  # 竞态检测（并发用例在 -race 下验证）
go test -v ./...              # 查看每个用例及其判定依据的日志
```

覆盖的业务场景：

- schema 兼容演进与各类破坏性变更（删列/收窄/跨族/收紧可空/改名）及逐项原因；
- 任意两版本差异、历史版本不可变；
- 键歧义、资产不存在、悬空字段三类可区分错误；自环、环、重复边、幂等撤销；
- 并发登记/撤销/查询下的快照一致性与幂等（竞态检测）；
- 影响面结果顺序稳定、与登记顺序无关、含传播路径、规模性能；
- 重启恢复、损坏文件按类型识别、软删除后历史可追溯。
