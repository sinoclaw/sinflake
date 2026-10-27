package sinflake

import (
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────
// 本文件把 sino-order-system 的 internal/pkg/orderid/v2.go 里
// nextSnow() 的算法**原样复刻**成 oracle，再与 sinflake 的
// LayoutMachine8Seq16 逐字节对拍。
// ─────────────────────────────────────────────────────────────

type v2Oracle struct {
	epoch   time.Time
	machine uint64
	last    uint64
	seq     uint64
	now     func() time.Time
}

const (
	oracleUnit       = 10 * time.Millisecond
	oracleMachineBit = 8
	oracleSeqBit     = 16
)

func newV2Oracle(epoch time.Time, machine uint64, now func() time.Time) *v2Oracle {
	return &v2Oracle{epoch: epoch, machine: machine & 0xFF, now: now}
}

// nextSnow 与 v2.go 一致：
//
//	now = max(now, last)；同拍则 seq++（超界就逻辑时钟 +1、seq 归零）；否则 seq=0
//	返回 now<<(8+16) | machine<<16 | seq
func (o *v2Oracle) nextSnow() uint64 {
	cur := uint64(o.now().Sub(o.epoch) / oracleUnit)
	if cur < o.last {
		cur = o.last // 时钟回拨：停在上一拍
	}
	if cur == o.last {
		o.seq++
		if o.seq > (1<<oracleSeqBit)-1 {
			o.last++
			cur = o.last
			o.seq = 0
		}
	} else {
		o.seq = 0
	}
	o.last = cur
	return cur<<(oracleMachineBit+oracleSeqBit) | o.machine<<oracleSeqBit | o.seq
}

// 把 sinflake 配成与订单系统同样的口径
func newLegacyLayoutGen(t *testing.T, epoch time.Time, machine uint16, now func() time.Time) *Sinflake {
	t.Helper()
	sf, err := New(Settings{
		StartTime: epoch,
		Layout:    LayoutMachine8Seq16,
		MachineID: func() (uint16, error) { return machine, nil },
		Now:       now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sf
}

// 逐字节对比：常规取号、同拍密集取号、序列溢出边界、时钟回拨
func TestLayoutParityWithOrderSystem(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	base := epoch.Add(1000 * time.Second)
	c := newClock(base)

	const machine = uint16(1)
	oracle := newV2Oracle(epoch, uint64(machine), c.Now)
	sf := newLegacyLayoutGen(t, epoch, machine, c.Now)

	total := 0
	compare := func(n int, ctx string) {
		for i := 0; i < n; i++ {
			total++
			want := oracle.nextSnow()
			got, err := sf.NextID()
			if err != nil {
				t.Fatalf("%s #%d: %v", ctx, i, err)
			}
			if got != want {
				t.Fatalf("%s #%d 不一致:\n  v2.go   = %d\n  sinflake= %d\n  差 = %d",
					ctx, i, want, got, int64(got)-int64(want))
			}
		}
	}

	// ① 常规：每拍取 3 个，前进 5 拍
	for tick := 0; tick < 5; tick++ {
		c.Set(base.Add(time.Duration(tick) * 10 * time.Millisecond))
		compare(3, "常规取号")
	}

	// ② 同拍密集：一拍内取 1000 个
	c.Set(base.Add(100 * time.Millisecond))
	compare(1000, "同拍密集")

	// ③ 序列溢出边界：16 位序列 = 65536 个/拍，跨过边界再取 200 个
	c.Set(base.Add(200 * time.Millisecond))
	compare(65536+200, "序列溢出边界")

	// ④ 时钟回拨 100ms：两边都不倒退、靠序列继续
	c.Set(base.Add(500 * time.Millisecond))
	compare(10, "回拨前的正常拍")
	c.Set(base.Add(400 * time.Millisecond))
	compare(10, "回拨 100ms")

	t.Logf("对拍通过：%d 个号与 v2.go nextSnow() 逐字节一致（含同拍密集、16 位序列溢出、100ms 回拨）", total)
}

// 线上真实单号在两种布局下的解号结果，证明「时间一致、机器号与序列互换」
func TestLayoutDecodeRealOrderIDs(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	type want struct {
		id      string
		snow    uint64
		machine uint64
		seq     uint64
		ts      string // UTC
	}
	cases := []want{
		// 10-19 那笔：代理 3071 / 商品 600014
		{"02UUNGvcEcFqD2bOtKchUk", 95153012258963518, 1, 62, "2026-10-19 10:20:14"},
		// 10-10 那笔：代理 4000 / 商品 100003
		{"02SMcKj42guNG1mgHkisKJ", 93844117771321381, 5, 37, "2026-10-10 09:37:33"},
	}

	for _, c := range cases {
		if got := MachineIDWithLayout(c.snow, LayoutMachine8Seq16); got != c.machine {
			t.Fatalf("%s 机器号(既有布局) = %d, want %d", c.id, got, c.machine)
		}
		if got := SequenceNumberWithLayout(c.snow, LayoutMachine8Seq16); got != c.seq {
			t.Fatalf("%s 序列号(既有布局) = %d, want %d", c.id, got, c.seq)
		}
		// sony 布局会把两个字段读反
		if got := MachineIDWithLayout(c.snow, LayoutSony); got != c.seq {
			t.Fatalf("%s sony 布局机器号应等于既有布局的序列号(%d), got %d", c.id, c.seq, got)
		}
		// 时间位一致，两边解出同一时刻
		ts := epoch.Add(ElapsedTime(c.snow)).Format("2006-01-02 15:04:05")
		if ts != c.ts {
			t.Fatalf("%s 时刻 = %s, want %s", c.id, ts, c.ts)
		}
	}
}

// 布局容量与机器号边界
func TestLayoutCapacityAndBounds(t *testing.T) {
	if got := LayoutSony.PerSecondCapacity(); got != 25600 {
		t.Fatalf("sony 容量 = %d, want 25600", got)
	}
	if got := LayoutMachine8Seq16.PerSecondCapacity(); got != 6553600 {
		t.Fatalf("machine8-seq16 容量 = %d, want 6553600", got)
	}
	if LayoutMachine8Seq16.MaxMachineID() != 255 {
		t.Fatal("machine8-seq16 机器号上限应为 255")
	}

	// 8 位机器号布局下给 300：必须拒绝，不能静默截断
	if _, err := New(Settings{
		StartTime: time.Now().Add(-time.Hour),
		Layout:    LayoutMachine8Seq16,
		MachineID: func() (uint16, error) { return 300, nil },
	}); err != ErrInvalidMachineID {
		t.Fatalf("want ErrInvalidMachineID, got %v", err)
	}

	// sony 布局仍允许 16 位机器号
	if _, err := New(Settings{
		StartTime: time.Now().Add(-time.Hour),
		Layout:    LayoutSony,
		MachineID: func() (uint16, error) { return 300, nil },
	}); err != nil {
		t.Fatalf("sony 布局应允许 300: %v", err)
	}
}

// 既有布局下的 Compose / Decompose 往返
func TestComposeLegacyLayout(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sf := newLegacyLayoutGen(t, epoch, 7, time.Now)

	id, err := Compose(sf, epoch.Add(2*time.Second), 60000, 200)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if got := sf.DecodeSequence(id); got != 60000 {
		t.Fatalf("序列 = %d, want 60000", got)
	}
	if got := sf.DecodeMachineID(id); got != 200 {
		t.Fatalf("机器号 = %d, want 200", got)
	}
	d := sf.Decompose(id)
	if d["time"] != 200 || d["sequence"] != 60000 || d["machine-id"] != 200 {
		t.Fatalf("Decompose = %v", d)
	}

	// 16 位序列允许到 65535，超过就报错
	if _, err := Compose(sf, epoch.Add(time.Second), 0, 1); err != nil {
		t.Fatalf("边界内应成功: %v", err)
	}
	sfSony, _ := New(Settings{StartTime: epoch, MachineID: func() (uint16, error) { return 1, nil }})
	if _, err := Compose(sfSony, epoch.Add(time.Second), 300, 1); err != ErrInvalidSequence {
		t.Fatalf("sony 布局序列 300 应越界, got %v", err)
	}
}
