# sinflake

纯 Go 的分布式唯一 ID 库：**位布局沿用 sony/sonyflake，能力对齐美团 Leaf**。

- 零第三方依赖（`go.mod` 里没有 require），只吃标准库
- 两种发号模式：**snowflake**（可解码）与 **号段 segment**（严格递增、不看时钟）
- 机器号可自动租约分配，不引 ZooKeeper
- 兼容 Leaf 的 `-1 / -2 / -3` 异常码与 HTTP 接口形状

```
go get github.com/sinoclaw/sinflake
```

## 一、snowflake 模式（sonyflake 兼容）

位布局与 sonyflake 逐位一致，两边发的号可以互解：

```
39 bits 时间（单位 10ms） | 8 bits 序列号 | 16 bits 机器号
```

```go
import "github.com/sinoclaw/sinflake"

sf, err := sinflake.New(sinflake.Settings{
    StartTime: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
    MachineID: func() (uint16, error) { return 1, nil },
})
if err != nil {
    log.Fatal(err)
}

id, err := sf.NextID()          // 单发
ids, err := sf.NextIDs(256)     // 批量（少拿锁）

sinflake.MachineID(id)          // 解机器号
sinflake.SequenceNumber(id)     // 解序列号
sinflake.ElapsedTime(id)        // 解相对纪元的时间
sinflake.Time(epoch, id)        // 解绝对时间
sinflake.Decompose(id)          // 全部拆开
sinflake.Compose(sf, t, 3, 1)   // 反向拼
```

与 sonyflake 同名同义的还有：`NewSinflake`、`BitLenTime/BitLenSequence/BitLenMachineID`、
`Settings{StartTime, MachineID, CheckMachineID}`、默认纪元 `2014-09-01 UTC`、默认机器号取私有网卡 IP 低 16 位。

### 位布局可选（与既有系统对齐）

时间永远在最高 39 位（10ms 一片），低 24 位有两种排法：

| 布局 | 低 24 位排法 | 单机上限 | 用途 |
|---|---|---|---|
| `LayoutSony`（默认） | 序列 8 位 \| 机器号 16 位 | ≈2.56 万/s | 与 sony/sonyflake 逐位一致 |
| `LayoutMachine8Seq16` | 机器号 8 位 \| 序列 16 位 | ≈655 万/s | 与 sino-order-system 既有单号口径逐位一致 |

```go
sf, _ := sinflake.New(sinflake.Settings{
    StartTime: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), // 必须与目标系统一致
    Layout:    sinflake.LayoutMachine8Seq16,
    MachineID: func() (uint16, error) { return 1, nil },     // 8 位布局只有 0~255
})
id, _ := sf.NextID()
sf.DecodeMachineID(id) // 按布局解，不会与 sony 布局混淆
```

两种布局解出的**时刻相同**（时间位一致），但机器号与序列会互换 —— 换布局前必须先做逐字节对拍
（本仓 `layout_test.go` 就是把订单系统 `nextSnow()` 当 oracle 的样例）。

**时钟回拨策略**是 Leaf 带过来的能力，默认保持 sonyflake 语义：

| 策略 | 行为 | 适用 |
|---|---|---|
| `Logical`（默认） | 不等、不退，把逻辑时间往前推一格继续发号 | 永不阻塞，sonyflake 原语义 |
| `WaitRetry` | 回拨 ≤ `MaxBackwardWait`（默认 5ms）时等 2×回拨量重试，超窗报 `ErrClockBackward` | 对齐 Leaf |
| `Strict` | 回拨即报错 | 资金类场景最保守 |

## 二、号段模式（Leaf segment 对齐）

```go
store, _ := sinflake.NewMySQLStore(db, "leaf_alloc")   // 也可用 sinflake.NewMemStore(...) 做单机
gen, _ := sinflake.NewSegmentGenerator(store,
    sinflake.WithMaxStep(1000000),                     // 与 Leaf 一致
    sinflake.WithSegmentDuration(15*time.Minute),      // 与 Leaf 一致
)
gen.Init(ctx)          // 装载 tag + 启动每分钟同步
defer gen.Close()

id, err := gen.NextID(ctx, "order")
ids, err := gen.NextIDs(ctx, "order", 100)
gen.Cache()            // 监控快照（Leaf 的 /cache）
gen.Allocs(ctx)        // 库里的号段配置
```

对齐了 Leaf 的这些点：

| Leaf 行为 | sinflake 实现 |
|---|---|
| 双 buffer（current + next） | `segmentBuffer.segs[2]`，`switchPos()` 切换 |
| 用到 90% 异步预取下一段 | `seg.idle() < 0.9*step` 触发 `tryAsyncPrefetch` |
| 15 分钟内用完 → step 翻倍（≤1e6）；30 分钟没用完 → 减半（≥库里 step） | `updateSegmentFromDB` 里按 `segmentDuration` 判定 |
| 每次领号一条 `UPDATE ... max_id = max_id + step` | `Store.UpdateMaxID`；`step<=0` 表示沿用表里的 step |
| 每分钟把库里新增/删除的 tag 同步进缓存 | `Init` 起的 ticker，默认 60s |
| 异常码 -1 未初始化 / -2 key 不存在 / -3 双段都空 | `ErrCacheNotInit` / `ErrKeyNotExists` / `ErrSegmentNotReady`，`ErrorCode(err)` 直接拿负数 |
| 首次取号时同步装载 buffer | `NextID` 内 `initOK` 双检 |

**与 Leaf 的一处有意差异**：Leaf 在「当前段耗尽且备段未就绪」时直接返回 -3；sinflake 会先**同步回库领一段**再发号，只有回库失败才报 -3。这样高并发下不会因为预取慢了就扔异常码。

`Store` 是接口，除了 MySQL 你也能接 Redis/PG/自研配置中心。

## 三、机器号租约（替代 Leaf 的 ZooKeeper）

Leaf 用 ZK 抢 workerId。sinflake 用一张小表，少一个外部运行时：

```go
p, _ := sinflake.NewMySQLWorkIDProvider(db, "sinflake_workid",
    sinflake.LocalInstanceName("order-service"), time.Minute, 65535)

id, err := p.Acquire(ctx)      // 抢号：先认领自己的，再从 0 往上找空位/过期号
stop := p.StartKeepAlive(20*time.Second)
defer stop()
defer p.Release(ctx)           // 优雅退出时让号可回收

sf, _ := sinflake.New(sinflake.Settings{
    StartTime: epoch,
    MachineID: sinflake.MachineIDFromProvider(ctx, p),
})
```

不用租约就退回 sonyflake 的默认做法：`sinflake.IPProvider{}` 或直接留空 `MachineID`。

## 四、表结构

```sql
-- 号段表（与 Leaf 完全一致，可直接复用 Leaf 的表）
CREATE TABLE leaf_alloc (
  biz_tag     varchar(128) NOT NULL DEFAULT '',
  max_id      bigint(20)   NOT NULL DEFAULT 1,
  step        int(11)      NOT NULL,
  description varchar(256) DEFAULT NULL,
  update_time timestamp    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (biz_tag)
) ENGINE=InnoDB;

INSERT INTO leaf_alloc (biz_tag, max_id, step, description) VALUES ('order', 1, 2000, '订单号');

-- 机器号租约表
CREATE TABLE sinflake_workid (
  machine_id smallint unsigned NOT NULL,
  instance   varchar(128) NOT NULL,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (machine_id),
  KEY idx_instance (instance)
) ENGINE=InnoDB;
```

## 五、HTTP 服务

```bash
go run ./cmd/sinflake-server -addr :8080 -mode both -step 2000 -machine 1 -keys order,pay
```

| 接口 | 说明 |
|---|---|
| `GET /api/segment/get/{key}` | 号段取号，返回纯文本 |
| `GET /api/segment/get/{key}/batch/{n}` | 批量取号，逗号分隔 |
| `GET /api/snowflake/get/{key}` | snowflake 取号 |
| `GET /api/segment/cache` | 号段缓存快照（JSON） |
| `GET /healthz` | 健康检查 |

服务默认用内存号段，**仅供联调压测**；生产请在自己的进程里接 `MySQLStore`。

## 六、边界与实测

实测（低功耗 N150 小主机）：

```
BenchmarkSinflakeNextID           38600 ns/op    ← 见下方说明
BenchmarkSinflakeNextIDs16         615 µs/op    （16 个一次，约 38 µs/个）
BenchmarkSegmentNextID             38.8 ns/op   ≈ 2500 万/s（缓存命中）
BenchmarkSegmentNextIDParallel      119 ns/op
```

**snowflake 的 38µs/op 不是慢，是布局的物理上限**：8 位序列号意味着**同一个 10ms 时间片最多发 256 个号**，
即单实例上限 **25,600 个/秒**；压满之后每次 `NextID` 只能 sleep 到下一个时间片，所以平均值就变成 38µs。
要更高就得改位分配（减机器位、加序列位），或改用号段模式——**号段模式不看时钟，缓存命中就是 38ns**。

测试覆盖：位布局与 sonyflake 逐位相等、Compose/Decompose 往返、2 万次唯一且单调、16 协程 × 3000 无重复、
三种时钟回拨策略、时间片越界、号段换段/并发/批量/tag 热更新/step 翻倍。
`go test -race ./...` 全绿。

## 七、License

MIT。位布局与 API 语义源自 [sony/sonyflake](https://github.com/sony/sonyflake)（MIT），
号段与伸缩策略参考 [Meituan-Dianping/Leaf](https://github.com/Meituan-Dianping/Leaf)（Apache-2.0）。

## 八、与 Leaf 的逐项对账（按 Leaf 源码核对）

### 已对齐

| Leaf 源码位置 | 能力 | sinflake |
|---|---|---|
| `SegmentIDGenImpl` | 双 buffer + `switchPos` | `segmentBuffer.segs[2]` |
| `SegmentIDGenImpl:207` | 90% 阈值异步预取 | `idle < 0.9*step` → `tryAsyncPrefetch` |
| `SegmentIDGenImpl:158-200` | step 15min 翻倍 / 30min 减半，上限 1_000_000 | `updateSegmentFromDB` 按 `segmentDuration` 判定 |
| `SegmentIDGenImpl:69-85` | 每分钟同步 tag 增删 | `Init` 里的 ticker（默认 60s） |
| `SegmentIDGenImpl:23-31` | 异常码 -1 / -2 / -3 | `ErrCacheNotInit` / `ErrKeyNotExists` / `ErrSegmentNotReady` + `ErrorCode()` |
| `SegmentIDGenImpl:260` | `waitAndSleep` 自旋后睡 10ms | `waitAndSpin` |
| `IDAllocMapper` | `UPDATE ... max_id = max_id + step` | `Store.UpdateMaxID`（`step<=0` 用表里的 step） |
| `IDGen` / `Result` / `Status` | `get(key)` 返回 `Result{id,status}` | `SegmentGenerator.Get` / `Sinflake.Get`，`Result.String()` 与 Leaf 同格式 |
| `LeafController` | 缺 key → 400（NoKeyException）；异常 → 500 且正文是 `Result{...}` | 已实测：`HTTP=400` / `HTTP=500 Result{id=-2, status=EXCEPTION}`；另加 `?raw=1` 回负数码 |
| `LeafMonitorController:/cache` | HTML 监控页 | `/cache` 出 HTML 表，`/api/segment/cache` 出 JSON |
| `SnowflakeIDGenImpl:71-96` | 回拨分档：超窗口 -3、等过仍落后 -1、中断 -2 | `ErrClockBackwardTooLarge/-Behind/-Interrupted` + `ClockErrorCode()` |
| `SnowflakeIDGenImpl:98-107` | 新 ms / 序列用尽时 `RANDOM.nextInt(100)` | `Settings.RandomSequenceStart`（默认关，保持 sony 语义） |

### 有意不同（不是欠账）

| Leaf | sinflake | 原因 |
|---|---|---|
| snowflake 位布局：41 bit 毫秒 + 10 bit workerId + 12 bit 序列，纪元 2010-11-04 | 沿用 sony 布局：39 bit 10ms + 8 bit 序列 + 16 bit 机器号，纪元 2014-09-01 | 按需求克隆 sonyflake，编号要与既有系统互解 |
| workerId 由 ZooKeeper 抢注（`SnowflakeZookeeperHolder`） | `sinflake_workid` 表租约（注册/心跳/过期回收） | 少一个外部运行时；语义等价 |
| Spring Boot + MyBatis + Thymeleaf | 库 + 标准库 `net/http` | 零框架依赖，能嵌进任意 Go 进程 |
| 段耗尽且备段未就绪 → 直接抛 -3 | 先同步回库补一段，只有回库失败才抛 -3 | 高并发下不因预取慢而扔异常 |

### 明确没对齐 / 需要知道的差

1. **吞吐差 160 倍**：Leaf snowflake 12 位序列 = 4096 个/毫秒 ≈ 409.6 万/s；sinflake（sony 布局）8 位序列 = 256 个/10ms = **2.56 万/s**。
   要不撞上限就减机器位、加序列位（会破坏 sony 兼容），或用号段模式（实测 38.8ns/op）。
2. **两套 -1/-2/-3 语义不同**：号段的 -1/-2/-3 是缓存/Key/双段，snowflake 的 -1/-2/-3 是回拨分档。Leaf 原版也这样，`ErrorCode()` 与 `ClockErrorCode()` 分开给你。
3. ~~MySQL 两条链路未在真实库上验证~~ → **已完成**，见第九节（sindb 9.7.2 真库回归，4 项全过，连跑 3 轮稳定）。
4. Leaf 的 `LeafServerException`/`NoKeyException` 类层次、`spring-boot` 配置项（`leaf.properties`）没有照搬，Go 侧用 error + flag 代替。

## 九、真库回归（MySQL 兼容库 9.7.2）

在一个独立的 `sinflake-it` module（，仅此处引 `go-sql-driver/mysql`，主库仍是零依赖）
通过 SSH 隧道把远端数据库映射到本地端口，只操作**独立库 `sinflake_test`**，不碰任何业务库。

```bash
# 回归模块不放本仓（它要引 MySQL 驱动，会破坏主库零依赖），单独放一处
cd ../sinflake-it
go test ./... -count=1 -v
```

4 个用例与结果：

| 用例 | 结论 |
|---|---|
| `TestMySQLStoreSegmentTwoInstances` | 两个「实例」各取 700，**全库唯一 1400 个**，各自严格递增，`leaf_alloc.max_id` 正确前移 |
| `TestMySQLStoreStepDoubling` | 快速消耗后步长 **100 → 400** 自适应（Leaf 的 15 分钟规则） |
| `TestMySQLWorkIDProvider` | 两实例拿到不同机器号（0 / 1）；同名实例重启**复用原号**；`Release` 后号可被回收 |
| `TestSnowflakeWithLeaseProvider` | 租约机器号 + 发号端到端：5000 个号唯一、机器号可反解、时刻可还原 |

连跑 3 轮全绿（26.8s）。

### 真库上抓到的两个 bug（已修，都是纸上测不出来的）

1. **`UPDATE ... max_id = max_id + ?` 后紧跟 `SELECT` 必须包在一个事务里。**
   否则并发下 A 执行完 UPDATE、B 又 UPDATE 一次，A 的 SELECT 读到的是 **B 加过之后的 max_id**，
   两个实例就会算出**重叠区间并发出同一批号**（实测 1400 个号里丢了 200 个，出现重复）。
   Leaf 的 `IDAllocDaoImpl` 用 autocommit=false 的 SqlSession 正是为了这个行锁，
   移植时漏掉事务就等于漏掉锁。修复：`BeginTx → UPDATE → SELECT → Commit`。
2. **机器号续约不能用 `RowsAffected` 判断归属。**
   MySQL 在「值没变化」时返回 0 行，而 `updated_at` 是**秒级**精度，
   同一秒内续约必然 0 行 → 被误判成「号被别的实例占了」，`KeepAlive` 直接报 `ErrMachineIDBusy`。
   修复：先 `SELECT instance` 校验归属，再刷新时间戳。

## 十、作为 SDK 接入

### 1. 安装

```bash
go get github.com/sinoclaw/sinflake@v0.1.0
```

主库**零第三方依赖**；只有你要用 `MySQLStore` / `MySQLWorkIDProvider` 时，才在你自己的
项目里引一个驱动（本库不替你绑驱动，避免把 `driver` 强塞进别人的依赖树）：

```go
import _ "github.com/go-sql-driver/mysql"
```

### 2. 三种接法，按场景挑

| 场景 | 用什么 | 要不要 DB |
|---|---|---|
| 单实例、只要能反解的号 | `sinflake.New` + `IPProvider` | 不要 |
| 多实例、要防撞号 | `sinflake.New` + `MySQLWorkIDProvider` 租约 | 要一张小表 |
| 高吞吐、要严格递增、不看时钟 | `SegmentGenerator` + `MySQLStore` | 要 `leaf_alloc` |
| 单机跑测试 | `SegmentGenerator` + `MemStore` | 不要 |

### 3. 错误处理约定

所有可判定的失败都是**哨兵错误**，直接返回、不做包装，用 `errors.Is` 判定：

```go
id, err := gen.NextID(ctx, "order")
switch {
case err == nil:
case errors.Is(err, sinflake.ErrClockBackwardTooLarge): // 时钟回拨超窗口
    // 告警，别重试太猛
case errors.Is(err, sinflake.ErrSegmentNotReady):       // 回库补段失败
    // 降级：返回错误给上游，别发重复号
default:
}
```

想拿 Leaf 的负数码：号段用 `sinflake.ErrorCode(err)`，snowflake 用 `sinflake.ClockErrorCode(err)`。

### 4. 兼容矩阵（实测）

| 项 | 版本 | 状态 |
|---|---|---|
| Go | 1.21+（本机 1.27.3 实测） | ✅ |
| MySQL 9.7 兼容实现（sindb 9.7.2） | ✅ 真库回归 4 项全过 |
| MySQL | 8.x / 9.x | ⚠️ 同族语法，未单独回归 |
| MariaDB / PostgreSQL | — | ❌ 未验证 |
| 第三方依赖 | 无 | ✅ `go.sum` 不存在 |

### 5. 版本与稳定性承诺

- 当前 `sinflake.Version = "0.1.0"`（`version.go`），可打进健康检查。
- **v0.x**：API 可能微调，但破坏性改动一定写进 `CHANGELOG.md` 并给迁移方式。
- **v1.0 之后**：遵守 semver，主版本内不破坏 `Settings` / `Sinflake` / `SegmentGenerator` / `Store` 的签名。
- 计数器语义（ID 单调、唯一范围）属于约定的一部分，不会在次版本里改。

### 6. 发布成公共 SDK 还差的硬条件

| 项 | 状态 |
|---|---|
| `LICENSE` | ✅ 已加（MIT） |
| `go.mod` 模块路径 | ✅ `github.com/sinoclaw/sinflake`（需推到对应仓库才可 `go get`） |
| 可运行 godoc 示例 | ✅ 4 个（`go test -run Example` 通过） |
| CHANGELOG / 版本号 | ✅ `CHANGELOG.md` + `version.go` |
| 仓库 + tag | ⚠️ 本地已 commit 并打 `v0.1.0` tag，**还没推到远端**（缺仓库地址） |
| CI（多 Go 版本 × go vet × test -race） | ⚠️ 未配置 |
| Go module proxy 收录 | ⚠️ 推 tag 后由 proxy 自动抓取（首次需 `go list -m` 触发） |
