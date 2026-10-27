# Changelog

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 semver。

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
