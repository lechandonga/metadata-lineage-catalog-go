# 本地元数据目录服务(Metadata & Lineage Catalog)

一个**纯本地、零外部依赖**的数据资产目录与字段级血缘服务。只使用 Go 标准库,
数据落在本地文件目录,通过 HTTP 查询,适合单机手工验证与本地开发。

- 数据资产与 schema 版本登记:稳定业务键、上报幂等、同版本内容冲突拒绝、历史版本不可变。
- Schema 演进的兼容性判定:加列/类型拓宽放行,删列/改名/收窄拒绝并标出原因。
- 字段级血缘:精确解析到已存在的资产与字段,悬空/歧义/环/自环均可区分。
- 下游影响面:结果与登记顺序无关,给出具体传播路径,数千资产/上万边下毫秒~秒级。
- 崩溃一致性:WAL 每条记录 fsync,半写记录重启自动截断,损坏数据按类型报错拒绝启动。

## 快速开始

```bash
# 启动(默认监听 127.0.0.1:8080,数据目录 ./data)
go run .

# 自定义地址与数据目录
go run . -addr 127.0.0.1:9090 -data-dir /tmp/mlc-data
```

健康检查:

```bash
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}
```

## HTTP 接口一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查 |
| POST | `/api/v1/assets` | 登记/演进资产版本 |
| GET | `/api/v1/assets?include_deleted=true` | 资产列表(默认不含已删除) |
| GET | `/api/v1/assets/{ns}/{asset}` | 资产详情(含全部历史版本) |
| DELETE | `/api/v1/assets/{ns}/{asset}` | 逻辑删除资产(打墓碑,幂等) |
| GET | `/api/v1/assets/{ns}/{asset}/versions/{v}` | 读取指定历史版本 |
| GET | `/api/v1/assets/{ns}/{asset}/diff?from=v1&to=v2` | 两版本差异 |
| POST | `/api/v1/edges` | 登记字段级血缘边 |
| DELETE | `/api/v1/edges` | 撤销血缘边(不存在也返回确定结果) |
| GET | `/api/v1/edges` | 现存血缘边列表 |
| GET | `/api/v1/impact?field=ns/asset.column` | 字段下游影响面与路径 |

### 登记资产版本

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/assets -d '{
  "namespace": "dw",
  "asset": "users",
  "version": "v1",
  "columns": [
    {"name": "id",   "type": "int64"},
    {"name": "name", "type": "string"}
  ]
}'
# {"asset_key":"dw/users","version":"v1","outcome":"created","compatible":true}
```

`outcome` 取值即判定依据:

- `created`:新版本已登记。
- `idempotent`:相同业务键 + 版本号 + 内容重复上报,未重复写入。
- `redundant`:新版本号但内容与最新版完全相同,仍按新版本保留。

### 登记 / 撤销血缘边

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/edges -d '{
  "source": {"namespace":"dw","asset":"users","column":"id"},
  "target": {"namespace":"dw","asset":"orders","column":"uid"}
}'

curl -s -X DELETE http://127.0.0.1:8080/api/v1/edges -d '{
  "source": {"namespace":"dw","asset":"users","column":"id"},
  "target": {"namespace":"dw","asset":"orders","column":"uid"}
}'
# {"existed":true,"outcome":"removed"}
# 对不存在的边撤销:{"existed":false,"outcome":"idempotent_not_found"}
```

### 查询影响面

```bash
curl -s "http://127.0.0.1:8080/api/v1/impact?field=dw/users.id"
```

返回受影响字段列表(按全限定名升序)以及到每个下游字段的一条**最短、
字典序最小**的具体路径,用于解释影响如何传播。

## 数据模型

### 资产(Asset)

- 稳定业务键:`(namespace, name)`,全限定形式 `namespace/name`。
- 一个资产包含一个**不可变**版本集合 `versions`,以及指向最新版本的 `latest`。
- `deleted` 为墓碑标记:删除后默认列表不可见,但历史版本与历史血缘仍保留可追溯。

### 版本(Version)

| 字段 | 说明 |
| --- | --- |
| `version` | 调用方给定的版本号(字符串) |
| `columns` | 列定义:`{name, type, description?}` |
| `fingerprint` | 对列集合做**与列顺序无关**的 SHA-256 指纹,用于幂等/冲突判定 |
| `created_at` | 登记时间 |
| `deleted` / `deleted_at` | 资产被删除后,历史版本带墓碑标记 |

幂等与冲突规则:

- 同 `(业务键, 版本号, 指纹)` 重复上报 → 幂等成功,不追加任何记录。
- 同 `(业务键, 版本号)` 但指纹不同 → `422 conflict`,**拒绝覆盖**,历史不可变。

### 血缘边(Edge)

边为有向字段依赖:`source 字段 → target 字段`,表示 source 的数据流入 target。
以 `source 全限定名 => target 全限定名` 去重。

## Schema 兼容判定规则

新版本始终与资产当前**最新版本**比较。类型名比较前做归一化
(去空格、转小写,例如 `INT64` 与 `int64` 相同)。

**兼容(放行)**

- 仅新增列。
- 同名列在数值族内拓宽:`int8 < int16 < int32 < int64/int < float32 < float64`。
- 仅列顺序变化(指纹与列顺序无关)。

**破坏性(拒绝登记,并在 `details` 中逐项标出原因)**

| kind | 含义 |
| --- | --- |
| `removed_column` | 列被删除,下游消费者读不到该字段 |
| `renamed_column` | 同类型"删一列 + 增一列"识别为改名;列名是血缘锚点,改名视为破坏 |
| `type_narrowed` | 类型收窄,或跨越数值族的任意类型变更(如 `string`→`int64`) |

任意两个已存在版本之间可用 `diff` 接口查看加了什么、删了什么、改了什么;
差异是双向可算的(从新版到旧版的 diff 会呈现为删列/收窄)。

## 血缘语义与影响面

**登记时的精确校验**(错误均可区分):

- `validation_error`(400):端点缺少 namespace/asset/column。
- `dangling_reference`(404):端点资产不存在,或资产最新版本中不存在该字段。
- `ambiguous_key`(409):只用资产短名定位且命中多个 namespace,要求显式指定 namespace。
- `asset_deleted`(410):端点资产已被删除(不允许建立新关联)。
- `conflict`(422):自环。
- `cycle_detected`(422):该边会使图中出现有向环(登记前做可达性检测)。

**幂等与撤销**

- 完全相同的边重复登记 → 幂等,不产生第二条边。
- 撤销存在的边 → `removed`;撤销不存在的边 → 确定结果 `idempotent_not_found`。
- 环在写入时被拒绝;遍历本身使用访问集合(BFS/DFS),即便图中存在多条汇聚路径
  也不会挂死或重复计数。

**影响面计算**

- 从给定字段出发沿 `source → target` 做 BFS,所有可达字段即下游影响面。
- 邻接按字段全限定名排序后遍历,保证结果与边的登记顺序无关、稳定有序。
- BFS 首次发现的前驱构成到每个下游的一条最短路径;在等长路径中按字典序最小选择。
- 新登记/撤销的边在互斥提交完成后立即对后续查询可见。
- 时间复杂度为 O(V+E);在 3000 资产 / 15000 边的基准下单次查询约 1~2 毫秒。

## 本地存储与落盘说明

数据目录(默认 `./data`)包含:

```
data/
├── wal.log        # 只追加的预写日志,每行一条 JSON 事件,每条追加后 fsync
└── snapshot.json  # 压缩时写出的完整状态快照(临时文件 + fsync + 原子 rename)
```

提交协议与崩溃恢复:

- 写路径在互斥区内**先持久化后改内存**:`Append` 写入整行并 `fsync` 成功,
  事件才算提交;fsync 失败则内存不变,不会出现"写了一半"的状态。
- 启动时先加载 `snapshot.json`,再按顺序重放 `wal.log`。
- WAL 末尾**没有换行符**的最后一条记录视为进程被杀留下的半写记录,
  重启时自动截断(该记录从未完成提交);截断后不影响此前已提交数据。
- WAL 中间任何无法解析或结构非法的行、快照整体无法解析,一律返回
  `corrupt_data` 错误并拒绝启动,绝不把坏数据当正常数据返回。
- 压缩(`Compact`)把完整状态原子写为快照并清空 WAL,随后新的写入继续追加 WAL;
  压缩过程中断不会同时丢失旧 WAL(快照以 rename 原子就位)。

并发模型:

- `sync.RWMutex` 保护内存状态。所有变更(登记/删除/加边/撤边)串行提交;
  查询在读锁下基于当前一致状态计算,不会看到写一半的资产、版本或血缘。
- 失败重试在语义上幂等:同内容重复登记不会产生重复版本或重复边。

## 兼容范围

- 单机单进程本地使用;不支持多进程同时写同一数据目录(无文件锁),
  也不包含任何外部系统(数据库、消息队列、网络元数据源)对接。
- 持久化格式(`wal.log` 行结构与 `snapshot.json`)面向本地运行与重启恢复,
  不承诺跨大版本的长期存储格式兼容;数据量级按题目要求设计(数千资产、上万边)。
- Go 版本与 module 声明沿用工程现有设置,仅使用 Go 标准库。

## 本地验证

```bash
# 全部单元测试
go test ./...

# 竞态检测(并发用例在 -race 下稳定通过,需要 CGO)
CGO_ENABLED=1 go test -race ./...

# 影响面性能基准(3000 资产 / 15000 边)
go test -run='^$' -bench=Impact ./catalog/

# 静态检查
go vet ./...
```

测试覆盖的业务场景(测试名即业务用例,运行 `-v` 可见每条用例的判定依据):

- schema 兼容与破坏性变更(`TestSchemaCompatibility`)。
- 版本差异、登记幂等与同版本冲突(`TestVersionDiff`、`TestRegisterIdempotencyAndConflict`)。
- 短名歧义与悬空引用(`TestAmbiguousKeyAndDanglingReference`)。
- 自环、重复边、环拒绝、撤销幂等、影响面与路径、顺序稳定性(`catalog/lineage_test.go`)。
- 并发读写的快照一致性、幂等与无重复边(`catalog/concurrency_test.go`)。
- 重启恢复、WAL 半写截断、损坏 WAL/快照识别、压缩恢复(`catalog/persistence_test.go`)。

## 目录结构

```
main.go                  本地 HTTP 服务入口(flag: -addr、-data-dir)
catalog/model.go         Asset / Version / Edge / FieldRef 等领域模型
catalog/errors.go        可区分错误类型(ErrorKind)
catalog/ref.go           业务键与字段引用解析
catalog/schema.go        类型拓宽规则、指纹、版本差异、不兼容识别
catalog/catalog.go       目录核心:登记、兼容判定、血缘图、影响面、事件 apply
catalog/event.go         WAL 事件与 Store 接口
catalog/wal.go           本地 WAL + fsync + 快照压缩 + 崩溃恢复/损坏识别
catalog/memstore.go      测试用内存 Store
catalog/server.go        net/http 接口层
```
