# Changelog

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 semver。

## [0.2.0] - 2026-10-27

### Added

- **`Layout` 位布局可选**，解决「同一份代码要服务两种既有口径」的问题：
  - `LayoutSony`（默认）：时间(39) | 序列(8) | 机器号(16)，与 sony/sonyflake 逐位一致，单机 ≈2.56 万/s。
  - `LayoutMachine8Seq16`：时间(39) | **机器号(8)** | **序列(16)**，与 sino-order-system 的订单号口径
    （`internal/pkg/orderid/v2.go` 的 `nextSnow()`）逐位一致，单机 ≈655 万/s。
  - `Layout` 上的辅助：`BitsSequence()` / `BitsMachine()` / `MaxMachineID()` /
    `PerTickCapacity()` / `PerSecondCapacity()`。
  - 按布局解码：包级 `SequenceNumberWithLayout` / `MachineIDWithLayout` / `DecomposeWithLayout`，
    生成器上 `DecodeSequence` / `DecodeMachineID` / `Decompose`（注意 `sf.MachineID()` 仍返回本实例自己的机器号）。
  - `Compose` 按生成器布局拼装；序列越界判断也按布局（sony 8 位 / machine8-seq16 16 位）。
  - 机器号超出布局范围（如 8 位布局给了 300）时返回 `ErrInvalidMachineID`，**不静默截断**。

### Verified

- **与既有订单系统的逐字节对拍**：把 `v2.go` 的 `nextSnow()` 原样复刻成 oracle，
  在生产同款参数（起点 2025-01-01 UTC、机器号 1、10ms 拍）下生成 **66,771 个号**，
  覆盖常规取号、同拍密集 1000 个、16 位序列溢出边界（65536+200）、100ms 时钟回拨，
  **全部与 sinflake 逐字节相同**。
- 线上真实单号反解：`02UUNGvcEcFqD2bOtKchUk` → 机器号 1 / 序列 62；`02SMcKj42guNG1mgHkisKJ` → 机器号 5 / 序列 37；
  两种布局解出的**时刻一致**（时间位相同），但机器号与序列在 sony 布局下会被读反 —— 这个用例同时钉住这一点。

## [0.1.0] - 2026-10-27

首个可发布版本。位布局与 API 语义沿用 [sony/sonyflake](https://github.com/sony/sonyflake)，
能力对齐 [Meituan-Dianping/Leaf](https://github.com/Meituan-Dianping/Leaf)。主库零第三方依赖。

### Added

- `Sinflake`（snowflake 模式）：39 bit 10ms + 8 bit 序列 + 16 bit 机器号，`New` / `NextID` /
  `NextIDs` / `Compose` / `Decompose` / `ElapsedTime` / `SequenceNumber` / `MachineID` / `Time`。
- 时钟回拨三策略：`Logical`（sony 语义，默认）、`WaitRetry`、`Strict`（Leaf 语义）；
  分档错误 `ErrClockBackwardTooLarge / -Behind / -Interrupted` 与 `ClockErrorCode()`。
- `Settings.RandomSequenceStart`：新时间片起始序列随机 0~99（对齐 Leaf 的 `RANDOM.nextInt(100)`）。
- `SegmentGenerator`（号段模式）：双 buffer、90% 异步预取、15min 步长翻倍 / 30min 减半（上限 1e6）、
  每分钟 tag 同步、`NextID` / `NextIDs` / `Cache` / `Allocs` / `Close`。
- `Store` 接口 + `MySQLStore`（表结构与 Leaf `leaf_alloc` 兼容）+ `MemStore`（单机/测试）。
- `WorkIDProvider` 接口 + `MySQLWorkIDProvider`（注册、心跳、过期回收，替代 Leaf 的 ZooKeeper）
  + `IPProvider`（sonyflake 默认的 IP 推导）。
- 对齐 Leaf 的 `Result` / `Status` 形状与 `Get(key)`；`ErrorCode()` 返号段 -1/-2/-3。
- `cmd/sinflake-server`：`/api/segment/get/{key}`、`/api/segment/get/{key}/batch/{n}`、
  `/api/snowflake/get/{key}`、`/api/segment/cache`、`/cache`（HTML）、`/healthz`。
- godoc 可运行示例 4 个（`ExampleNew`、`ExampleSegmentGenerator`、`ExampleDecompose`、`ExampleMachineIDFromProvider`）。

### Fixed

- **`MySQLStore.UpdateMaxID` 必须在事务里 UPDATE + SELECT**：只发两条语句时，并发下
  后一条 SELECT 会读到别的实例加过的 `max_id`，导致两个实例发出重叠区间的号
  （真库回归实测 1400 个号丢 200 个）。Leaf 用 autocommit=false 的 SqlSession 正是这个行锁。
- **`MySQLWorkIDProvider` 续约不能用 `RowsAffected` 判断归属**：`updated_at` 秒级精度，
  同一秒内续约 0 行会被误判为「号被占用」。改为先查归属再刷新。
- 号段模式在「当前段耗尽、备段未就绪」时改为**同步回库补一段**，而不是像 Leaf 那样直接返回 -3。
- `segmentBuffer` 内的 `initOK` / `step` / `minStep` / `updateTimestamp` 全部收进锁或原子量，
  `go test -race` 通过。

### Verified

- 单元测试：位布局与 sonyflake 逐位相等、Compose/Decompose 往返、2 万次唯一单调、
  16 协程 × 3000 无重复、三种回拨策略、时间片越界、号段换段/并发/批量/tag 热更新/步长翻倍。
- 真库回归（sindb 9.7.2，独立库 `sinflake_test`）：双实例各 700 全库唯一、
  步长 100→400、机器号租约分配/复用/回收、租约 + 发号 5000 个号可反解。连跑 3 轮绿。

### Known limitations

- MySQL 链路只在 MySQL 兼容的 **sindb 9.7.2** 上验证过，未在 MariaDB / PostgreSQL 上回归。
- 主库不绑定任何 SQL 驱动，`MySQLStore` 需调用方自行 import 驱动并传 `*sql.DB`。
- 8 位序列上限 2.56 万 ID/s（sony 布局固有）；要更高吞吐请用号段模式。
