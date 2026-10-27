package sinflake

// Layout 决定 64 位 ID 里「序列」与「机器号」的位置。
//
// 时间永远占最高的 39 位（10ms 一片），两种布局只在低 24 位不同：
//
//	LayoutSony（默认，sony/sonyflake 官方布局）
//	  63                        24 23        16 15              0
//	  +---------------------------+------------+-----------------+
//	  |      elapsed (39bit)      | seq (8bit) |  machine (16bit)|
//	  +---------------------------+------------+-----------------+
//	  ⇒ 每 10ms 最多 256 个号（单机 ≈2.56 万/s）
//
//	LayoutMachine8Seq16（sino-order-system 的既有布局）
//	  63                        24 23        16 15              0
//	  +---------------------------+------------+-----------------+
//	  |      elapsed (39bit)      | mach (8bit)|  seq   (16bit)  |
//	  +---------------------------+------------+-----------------+
//	  ⇒ 每 10ms 最多 65536 个号（单机 ≈655 万/s）
//
// 两者时间位一致 ⇒ 同一个号在两种布局下解出的**时刻相同**，但机器号与序列会互换。
// 已有系统切换布局会让历史号解析口径变化，务必先跑逐字节对比。
type Layout int

const (
	// LayoutSony sony/sonyflake 官方布局：时间(39) | 序列(8) | 机器号(16)。默认。
	LayoutSony Layout = iota
	// LayoutMachine8Seq16 时间(39) | 机器号(8) | 序列(16)，与 sino-order-system 的订单号口径一致。
	LayoutMachine8Seq16
)

// String 便于日志与错误信息。
func (l Layout) String() string {
	switch l {
	case LayoutMachine8Seq16:
		return "machine8-seq16"
	default:
		return "sony-seq8-machine16"
	}
}

// BitsSequence 返回该布局下序列号占用的位数。
func (l Layout) BitsSequence() int {
	if l == LayoutMachine8Seq16 {
		return 16
	}
	return 8
}

// BitsMachine 返回该布局下机器号占用的位数。
func (l Layout) BitsMachine() int {
	if l == LayoutMachine8Seq16 {
		return 8
	}
	return 16
}

// MaxMachineID 返回该布局允许的最大机器号（含）。
func (l Layout) MaxMachineID() uint16 {
	if l == LayoutMachine8Seq16 {
		return 1<<8 - 1
	}
	return 1<<16 - 1
}

// PerTickCapacity 返回该布局在单个 10ms 时间片内可发的号数。
func (l Layout) PerTickCapacity() int {
	return 1 << l.BitsSequence()
}

// PerSecondCapacity 返回该布局的理论单机每秒上限。
func (l Layout) PerSecondCapacity() int {
	return l.PerTickCapacity() * 100
}

func (l Layout) seqMask() uint16 {
	return uint16(1<<l.BitsSequence() - 1)
}

// ——— 布局相关的解码辅助 ———

// SequenceNumberWithLayout 按指定布局解出序列号。
func SequenceNumberWithLayout(id uint64, l Layout) uint64 {
	if l == LayoutMachine8Seq16 {
		return id & 0xFFFF
	}
	return (id >> 16) & 0xFF
}

// MachineIDWithLayout 按指定布局解出机器号。
func MachineIDWithLayout(id uint64, l Layout) uint64 {
	if l == LayoutMachine8Seq16 {
		return (id >> 16) & 0xFF
	}
	return id & 0xFFFF
}

// DecomposeWithLayout 按指定布局拆解 ID。
func DecomposeWithLayout(id uint64, l Layout) map[string]uint64 {
	return map[string]uint64{
		"id":         id,
		"msb":        id >> 63,
		"time":       elapsedTime(id),
		"sequence":   SequenceNumberWithLayout(id, l),
		"machine-id": MachineIDWithLayout(id, l),
	}
}

// DecodeSequence 按本生成器的布局解出序列号（注意与 sf.MachineID() 区分：
// 那个返回本实例自己的机器号，这个是解别人的号）。
func (sf *Sinflake) DecodeSequence(id uint64) uint64 { return SequenceNumberWithLayout(id, sf.layout) }

// DecodeMachineID 按本生成器的布局解出机器号。
func (sf *Sinflake) DecodeMachineID(id uint64) uint64 { return MachineIDWithLayout(id, sf.layout) }

// Decompose 按本生成器的布局拆解 ID。
func (sf *Sinflake) Decompose(id uint64) map[string]uint64 { return DecomposeWithLayout(id, sf.layout) }

// Layout 返回本生成器使用的布局。
func (sf *Sinflake) Layout() Layout { return sf.layout }
