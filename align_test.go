package sinflake

import (
	"context"
	"testing"
	"time"
)

// 对齐 Leaf：新时间片起始序列号随机（0~99），默认关闭时必须是 0。
func TestRandomSequenceStart(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	c := newClock(base)

	sf, err := New(Settings{
		StartTime:           base.Add(-time.Hour),
		MachineID:           fixedMachine(1),
		RandomSequenceStart: true,
		Now:                 c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}

	seqs := make(map[uint64]struct{}, 20)
	for i := 0; i < 20; i++ {
		// 每次往前跳 10ms，逼出「新时间片」分支
		c.Set(base.Add(time.Duration(i) * 10 * time.Millisecond))
		id, err := sf.NextID()
		if err != nil {
			t.Fatal(err)
		}
		seqs[SequenceNumber(id)] = struct{}{}
	}
	if len(seqs) < 2 {
		t.Fatalf("random sequence start not working, all same: %v", seqs)
	}
	for s := range seqs {
		if s > 99 && s != uint64(1<<BitLenSequence-1) {
			t.Fatalf("sequence %d out of 0..99 range", s)
		}
	}

	// 默认关闭时首个序列号必须是 0
	sf2, err := New(Settings{
		StartTime: base.Add(-time.Hour),
		MachineID: fixedMachine(1),
		Now:       c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Set(base.Add(time.Hour))
	id, err := sf2.NextID()
	if err != nil {
		t.Fatal(err)
	}
	if got := SequenceNumber(id); got != 0 {
		t.Fatalf("default sequence start = %d, want 0", got)
	}
}

// 对齐 Leaf snowflake：回拨超窗口 → -3；等过仍落后 → -1。
func TestClockErrorCodes(t *testing.T) {
	if ClockErrorCode(ErrClockBackwardTooLarge) != -3 {
		t.Fatal("ErrClockBackwardTooLarge should map to -3")
	}
	if ClockErrorCode(ErrClockBackwardBehind) != -1 {
		t.Fatal("ErrClockBackwardBehind should map to -1")
	}
	if ClockErrorCode(ErrClockBackwardInterrupted) != -2 {
		t.Fatal("ErrClockBackwardInterrupted should map to -2")
	}
	if ClockErrorCode(nil) != 0 {
		t.Fatal("nil should map to 0")
	}

	base := time.Now().Add(-time.Hour)
	c := newClock(base)
	sf, err := New(Settings{
		StartTime:       base.Add(-time.Hour),
		MachineID:       fixedMachine(1),
		BackwardPolicy:  Strict,
		MaxBackwardWait: 5 * time.Millisecond,
		Now:             c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sf.NextID(); err != nil {
		t.Fatal(err)
	}
	c.Set(base.Add(-time.Second))
	if _, err := sf.NextID(); err != ErrClockBackwardTooLarge {
		t.Fatalf("want ErrClockBackwardTooLarge, got %v", err)
	}
}

// 对齐 Leaf 的 Result/Status 形状
func TestResultShape(t *testing.T) {
	ctx := context.Background()
	g, err := NewSegmentGenerator(newRecStore(10))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	ok := g.Get(ctx, "order")
	if !ok.OK() || ok.ID <= 0 {
		t.Fatalf("bad success result: %+v", ok)
	}
	if got := ok.String(); got[:6] != "Result" {
		t.Fatalf("String() = %q, want Result{...} shape", got)
	}

	bad := g.Get(ctx, "missing")
	if bad.OK() || bad.ID != -2 {
		t.Fatalf("bad exception result: %+v", bad)
	}
	if got := bad.String(); got != "Result{id=-2, status=EXCEPTION}" {
		t.Fatalf("String() = %q", got)
	}

	sf, err := New(Settings{StartTime: time.Now().Add(-time.Hour), MachineID: fixedMachine(3)})
	if err != nil {
		t.Fatal(err)
	}
	if r := sf.Get("anything"); !r.OK() || r.ID == 0 {
		t.Fatalf("snowflake Get: %+v", r)
	}
}
