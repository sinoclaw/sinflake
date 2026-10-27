// Package sinflake 是一个纯 Go 的分布式唯一 ID 库。
//
// 它把两家的做法合成一套：
//
//   - 位布局、Settings / NextID / Compose / Decompose 语义沿用 sony/sonyflake
//     （39 位 10ms 时间片 + 8 位序列 + 16 位机器号）；
//   - 能力对齐美团 Leaf：号段模式（双 buffer 预取、step 自适应）、机器号租约分配、
//     Leaf 风格的 -1/-2/-3 异常码、批量取号、可插拔 BackwardPolicy（Leaf 的时钟回拨策略）。
//
// 一个 Sinflake ID 由三部分组成：
//
//	39 bits 时间（单位 10ms）
//	 8 bits 序列号
//	16 bits 机器号
package sinflake

import (
	"math/rand"
	"net"
	"sync"
	"time"
)

// ID 各段位长，与 sonyflake 完全一致，保证两边编号可互解。
const (
	BitLenTime      = 39
	BitLenSequence  = 8
	BitLenMachineID = 63 - BitLenTime - BitLenSequence // 16
)

const (
	// timeUnit 一个时间片的纳秒数（10ms）。
	timeUnit      int64 = 1e7
	nanoPerSecond       = int64(time.Second)
	// defaultStartTime 与 sonyflake 一致的默认纪元。
	defaultStartTime = "2014-09-01T00:00:00Z"
	// defaultMaxBackwardWait 默认回拨容忍窗口，对齐 Leaf（5ms 内等待重试）。
	defaultMaxBackwardWait = 5 * time.Millisecond
)

// BackwardPolicy 决定时钟回拨时怎么做。
type BackwardPolicy int

const (
	// Logical 与 sonyflake 一致：不等待、不回退，把逻辑时间往前推一格继续发号。
	// 好处是永不停服；代价是回拨期间发出的号，其时间片会比真实时间略快。
	Logical BackwardPolicy = iota
	// WaitRetry 与美团 Leaf 一致：回拨量 ≤ MaxBackwardWait 时等待 2×回拨量后重试，
	// 超过窗口则返回 ErrClockBackward。
	WaitRetry
	// Strict 回拨即报错，最保守，适合对号段顺序敏感的资金场景。
	Strict
)

// Settings 配置 Sinflake。
//
// StartTime 是纪元，零值用 sonyflake 的 2014-09-01 UTC；StartTime 在未来会返回
// ErrStartTimeAhead。
//
// MachineID 返回实例的唯一机器号（0~65535）；为 nil 时用私有网卡 IP 的低 16 位推导
// （与 sonyflake 一致）。需要多实例防撞时用 workid.go 里的租约分配器。
//
// CheckMachineID 可选，返回 false 表示机器号不可用。
//
// BackwardPolicy / MaxBackwardWait 控制时钟回拨行为，默认 Logical。
//
// Now 仅用于注入测试时钟，生产留空即可。
type Settings struct {
	StartTime       time.Time
	MachineID       func() (uint16, error)
	CheckMachineID  func(uint16) bool
	BackwardPolicy  BackwardPolicy
	MaxBackwardWait time.Duration
	// Layout 选择位布局：默认 LayoutSony（sony 官方：时间39|序列8|机器号16）；
	// 要与 sino-order-system 的既有单号口径对齐时用 LayoutMachine8Seq16（时间39|机器号8|序列16）。
	Layout Layout
	// RandomSequenceStart 对齐 Leaf：每个新时间片的起始序列号随机取 0~99，
	// 序列用尽后也重新随机，避免 ID 连号被外部推算。默认 false（保持 sonyflake 语义）。
	RandomSequenceStart bool
	Now                 func() time.Time
}

// Sinflake 是并发安全的分布式唯一 ID 生成器（snowflake 模式）。
type Sinflake struct {
	mu sync.Mutex

	startTime   int64 // 10ms 单位
	elapsedTime int64 // 10ms 单位，单调不减
	sequence    uint16
	machineID   uint16

	policy      BackwardPolicy
	layout      Layout
	maxBackward time.Duration
	randomStart bool
	rnd         *rand.Rand
	now         func() time.Time
}

var defaultInterfaceAddrs = net.InterfaceAddrs

// New 按 Settings 创建 Sinflake。以下情况返回错误：
//   - StartTime 晚于当下；
//   - MachineID 返回错误；
//   - CheckMachineID 返回 false。
func New(st Settings) (*Sinflake, error) {
	now := st.Now
	if now == nil {
		now = time.Now
	}
	if st.StartTime.After(now()) {
		return nil, ErrStartTimeAhead
	}

	sf := &Sinflake{
		sequence:    st.Layout.seqMask(), // 首号必然触发「新时间片」分支
		policy:      st.BackwardPolicy,
		layout:      st.Layout,
		maxBackward: st.MaxBackwardWait,
		randomStart: st.RandomSequenceStart,
		rnd:         rand.New(rand.NewSource(now().UnixNano())),
		now:         now,
	}
	if sf.maxBackward <= 0 {
		sf.maxBackward = defaultMaxBackwardWait
	}

	if st.StartTime.IsZero() {
		epoch, _ := time.Parse(time.RFC3339, defaultStartTime)
		sf.startTime = toSinflakeTime(epoch)
	} else {
		sf.startTime = toSinflakeTime(st.StartTime)
	}

	var err error
	if st.MachineID == nil {
		sf.machineID, err = lower16BitPrivateIP(defaultInterfaceAddrs)
	} else {
		sf.machineID, err = st.MachineID()
	}
	if err != nil {
		return nil, err
	}
	if st.CheckMachineID != nil && !st.CheckMachineID(sf.machineID) {
		return nil, ErrInvalidMachineID
	}
	if sf.machineID > sf.layout.MaxMachineID() {
		// 例如用 machine8-seq16 布局却给了 300：装不下，直接拒绝而不是截断
		return nil, ErrInvalidMachineID
	}
	return sf, nil
}

// NewSinflake 与 New 相同，但失败时返回 nil（兼容 sonyflake.NewSonyflake 的写法）。
func NewSinflake(st Settings) *Sinflake {
	sf, _ := New(st)
	return sf
}

// NextID 生成下一个唯一 ID；时间片用尽后返回 ErrOverTimeLimit。
func (sf *Sinflake) NextID() (uint64, error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	if sf.policy == Logical {
		return sf.nextLogical()
	}
	return sf.nextStrict()
}

// NextIDs 一次取 n 个 ID，比循环调用 NextID 少拿锁，适合批量建单。
func (sf *Sinflake) NextIDs(n int) ([]uint64, error) {
	if n <= 0 {
		return nil, ErrInvalidBatchSize
	}
	sf.mu.Lock()
	defer sf.mu.Unlock()

	out := make([]uint64, 0, n)
	for i := 0; i < n; i++ {
		var (
			id  uint64
			err error
		)
		if sf.policy == Logical {
			id, err = sf.nextLogical()
		} else {
			id, err = sf.nextStrict()
		}
		if err != nil {
			return out, err
		}
		out = append(out, id)
	}
	return out, nil
}

// nextLogical 完全沿用 sonyflake 的算法：时间片不倒退，序列溢出时把逻辑时间推进一格。
func (sf *Sinflake) nextLogical() (uint64, error) {
	maskSequence := sf.layout.seqMask()

	current := sf.currentElapsedTime()
	if sf.elapsedTime < current {
		sf.elapsedTime = current
		sf.sequence = sf.startSequence()
	} else {
		sf.sequence = (sf.sequence + 1) & maskSequence
		if sf.sequence == 0 {
			if sf.randomStart {
				sf.sequence = sf.startSequence()
			}
			sf.elapsedTime++
			overtime := sf.elapsedTime - current
			time.Sleep(sleepTime(overtime))
		}
	}
	return sf.toID()
}

// nextStrict 对齐美团 Leaf：回拨在窗口内等待重试，窗口外报错；序列用尽等下一时间片。
func (sf *Sinflake) nextStrict() (uint64, error) {
	maskSequence := sf.layout.seqMask()

	for {
		current := sf.currentElapsedTime()
		if current < sf.elapsedTime {
			offset := time.Duration(sf.elapsedTime-current) * testDurationUnit
			if sf.policy == Strict || offset > sf.maxBackward {
				// Leaf snowflake：回拨超过窗口直接 -3
				return 0, ErrClockBackwardTooLarge
			}
			time.Sleep(2 * offset) // 与 Leaf 的 wait(offset<<1) 同义
			if sf.currentElapsedTime() < sf.elapsedTime {
				// Leaf snowflake：等过之后仍落后 → -1
				return 0, ErrClockBackwardBehind
			}
			continue
		}
		if current == sf.elapsedTime {
			sf.sequence = (sf.sequence + 1) & maskSequence
			if sf.sequence == 0 {
				if sf.randomStart {
					sf.sequence = sf.startSequence()
				}
				sf.elapsedTime++ // 同一时间片内 256 个号用尽，推进一格
			}
		} else {
			sf.elapsedTime = current
			sf.sequence = sf.startSequence()
		}
		return sf.toID()
	}
}

// startSequence 返回一个新时间片的起始序列号：默认 0，开了 RandomSequenceStart 就取 0~99。
func (sf *Sinflake) startSequence() uint16 {
	if !sf.randomStart {
		return 0
	}
	return uint16(sf.rnd.Intn(100)) // 与 Leaf 的 RANDOM.nextInt(100) 一致
}

// testDurationUnit 一个时间片对应的 Duration（10ms），抽出来便于测试覆盖。
var testDurationUnit = 10 * time.Millisecond

func (sf *Sinflake) currentElapsedTime() int64 {
	return toSinflakeTime(sf.now()) - sf.startTime
}

func (sf *Sinflake) toID() (uint64, error) {
	if sf.elapsedTime >= 1<<BitLenTime {
		return 0, ErrOverTimeLimit
	}
	base := uint64(sf.elapsedTime) << (BitLenSequence + BitLenMachineID)
	if sf.layout == LayoutMachine8Seq16 {
		return base | uint64(sf.machineID&0xFF)<<16 | uint64(sf.sequence), nil
	}
	return base | uint64(sf.sequence&0xFF)<<BitLenMachineID | uint64(sf.machineID), nil
}

// MachineID 返回本实例的机器号。
func (sf *Sinflake) MachineID() uint16 { return sf.machineID }

func toSinflakeTime(t time.Time) int64 { return t.UTC().UnixNano() / timeUnit }

func sleepTime(overtime int64) time.Duration {
	return time.Duration(overtime*timeUnit) - time.Duration(time.Now().UTC().UnixNano()%timeUnit)
}

// —— 解码辅助，与 sonyflake 同名同义 ——

// ElapsedTime 返回 ID 生成时相对纪元经过的时间。
func ElapsedTime(id uint64) time.Duration {
	return time.Duration(int64(elapsedTime(id)) * timeUnit)
}

func elapsedTime(id uint64) uint64 { return id >> (BitLenSequence + BitLenMachineID) }

// SequenceNumber 返回 ID 的序列号。
func SequenceNumber(id uint64) uint64 {
	const maskSequence = uint64((1<<BitLenSequence - 1) << BitLenMachineID)
	return id & maskSequence >> BitLenMachineID
}

// MachineID 返回 ID 的机器号。
func MachineID(id uint64) uint64 {
	const maskMachineID = uint64(1<<BitLenMachineID - 1)
	return id & maskMachineID
}

// Time 把 ID 还原成绝对时间（纪元 + 时间片）。
func Time(startTime time.Time, id uint64) time.Time {
	if startTime.IsZero() {
		startTime, _ = time.Parse(time.RFC3339, defaultStartTime)
	}
	return startTime.UTC().Add(ElapsedTime(id))
}

// Compose 用组件拼出 ID（与 sonyflake.Compose 一致）。
func Compose(sf *Sinflake, t time.Time, sequence uint16, machineID uint16) (uint64, error) {
	elapsed := toSinflakeTime(t.UTC()) - sf.startTime
	if elapsed < 0 {
		return 0, ErrStartTimeAhead
	}
	if elapsed >= 1<<BitLenTime {
		return 0, ErrOverTimeLimit
	}
	if uint32(sequence) >= uint32(1<<sf.layout.BitsSequence()) {
		return 0, ErrInvalidSequence
	}
	base := uint64(elapsed) << (BitLenSequence + BitLenMachineID)
	if sf.layout == LayoutMachine8Seq16 {
		return base | uint64(machineID&0xFF)<<16 | uint64(sequence), nil
	}
	return base | uint64(sequence)<<BitLenMachineID | uint64(machineID), nil
}

// Decompose 拆解 ID 的各段。
func Decompose(id uint64) map[string]uint64 {
	return map[string]uint64{
		"id":         id,
		"msb":        id >> 63,
		"time":       elapsedTime(id),
		"sequence":   SequenceNumber(id),
		"machine-id": MachineID(id),
	}
}

// —— 默认机器号推导（与 sonyflake 一致）——

func privateIPv4(addrs func() ([]net.Addr, error)) (net.IP, error) {
	as, err := addrs()
	if err != nil {
		return nil, err
	}
	for _, a := range as {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ip := ipnet.IP.To4()
		if isPrivateIPv4(ip) {
			return ip, nil
		}
	}
	return nil, ErrNoPrivateAddress
}

func isPrivateIPv4(ip net.IP) bool {
	return ip != nil &&
		(ip[0] == 10 ||
			ip[0] == 172 && ip[1] >= 16 && ip[1] < 32 ||
			ip[0] == 192 && ip[1] == 168 ||
			ip[0] == 169 && ip[1] == 254)
}

func lower16BitPrivateIP(addrs func() ([]net.Addr, error)) (uint16, error) {
	ip, err := privateIPv4(addrs)
	if err != nil {
		return 0, err
	}
	return uint16(ip[2])<<8 + uint16(ip[3]), nil
}
