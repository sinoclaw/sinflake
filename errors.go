package sinflake

import "errors"

var (
	// ErrStartTimeAhead 起始时间在未来（与 sonyflake 同名同义）。
	ErrStartTimeAhead = errors.New("sinflake: start time is ahead of now")
	// ErrNoPrivateAddress 找不到私有网卡地址，无法推导机器号。
	ErrNoPrivateAddress = errors.New("sinflake: no private ip address")
	// ErrOverTimeLimit 39 位时间片用尽（约 69.7 年）。
	ErrOverTimeLimit = errors.New("sinflake: over the time limit")
	// ErrInvalidMachineID 机器号非法或校验不通过。
	ErrInvalidMachineID = errors.New("sinflake: invalid machine id")
	// ErrInvalidSequence 序列号越界。
	ErrInvalidSequence = errors.New("sinflake: invalid sequence number")
	// ErrInvalidBatchSize 批量取号数量非法。
	ErrInvalidBatchSize = errors.New("sinflake: invalid batch size")

	// —— 以下三个错误码与美团 Leaf 的异常码对齐 ——
	// ErrCacheNotInit ID 缓存未初始化成功，对应 Leaf EXCEPTION_ID_IDCACHE_INIT_FALSE(-1)。
	ErrCacheNotInit = errors.New("sinflake: id cache is not init ok")
	// ErrKeyNotExists 号段 key 不存在，对应 Leaf EXCEPTION_ID_KEY_NOT_EXISTS(-2)。
	ErrKeyNotExists = errors.New("sinflake: key does not exist")
	// ErrSegmentNotReady 双 buffer 都未装载好，对应 Leaf EXCEPTION_ID_TWO_SEGMENTS_ARE_NULL(-3)。
	ErrSegmentNotReady = errors.New("sinflake: both segments are not ready")
	// ErrClockBackwardTooLarge 时钟回拨超过容忍窗口，对应 Leaf snowflake 的 -3。
	ErrClockBackwardTooLarge = errors.New("sinflake: clock moved backwards beyond tolerance")
	// ErrClockBackwardBehind 等过之后仍然落后，对应 Leaf snowflake 的 -1。
	ErrClockBackwardBehind = errors.New("sinflake: still behind last timestamp after waiting")
	// ErrClockBackwardInterrupted 等待被中断，对应 Leaf snowflake 的 -2（Go 侧极少触发，保留语义）。
	ErrClockBackwardInterrupted = errors.New("sinflake: wait interrupted")
	// ErrClockBackward 保留旧名，等价于 ErrClockBackwardTooLarge。
	ErrClockBackward = ErrClockBackwardTooLarge
	// ErrNoMachineIDProvider 没有配置机器号来源。
	ErrNoMachineIDProvider = errors.New("sinflake: no machine id provider")
	// ErrMachineIDBusy 机器号已被别的实例占用且租约未过期。
	ErrMachineIDBusy = errors.New("sinflake: machine id is occupied by another instance")
)

// Leaf 的异常码常量，方便调用方对账。
const (
	ExceptionIDCacheNotInit = -1
	ExceptionIDKeyNotExists = -2
	ExceptionIDTwoSegments  = -3
)

// ClockErrorCode 把时钟回拨类错误映射成 Leaf snowflake 的负数码：
// -1 等过仍落后，-2 被中断，-3 回拨超窗口。与号段模式的 -1/-2/-3 是两套不同的语义，
// 这一点 Leaf 自身也是如此，调用方别混用。
func ClockErrorCode(err error) int64 {
	switch err {
	case ErrClockBackwardBehind:
		return -1
	case ErrClockBackwardInterrupted:
		return -2
	case ErrClockBackwardTooLarge:
		return -3
	}
	return 0
}

// ErrorCode 把 sentinel error 映射成 Leaf 风格的负数异常码；未知错误返回 0。
func ErrorCode(err error) int64 {
	switch err {
	case ErrCacheNotInit:
		return ExceptionIDCacheNotInit
	case ErrKeyNotExists:
		return ExceptionIDKeyNotExists
	case ErrSegmentNotReady:
		return ExceptionIDTwoSegments
	}
	return 0
}
