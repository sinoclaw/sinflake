package sinflake

import (
	"sync"
	"testing"
	"time"
)

func fixedMachine(id uint16) func() (uint16, error) {
	return func() (uint16, error) { return id, nil }
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func TestBitLayoutMatchesSonyflake(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sf, err := New(Settings{StartTime: base, MachineID: fixedMachine(7)})
	if err != nil {
		t.Fatal(err)
	}

	// 相对纪元 1 秒 = 100 个时间片
	id, err := Compose(sf, base.Add(time.Second), 3, 7)
	if err != nil {
		t.Fatal(err)
	}

	if got := elapsedTime(id); got != 100 {
		t.Fatalf("elapsed = %d, want 100", got)
	}
	if got := SequenceNumber(id); got != 3 {
		t.Fatalf("sequence = %d, want 3", got)
	}
	if got := MachineID(id); got != 7 {
		t.Fatalf("machine id = %d, want 7", got)
	}

	// 手工按 sonyflake 的位运算拼一遍，必须逐位相等
	want := uint64(100)<<(BitLenSequence+BitLenMachineID) | uint64(3)<<BitLenMachineID | uint64(7)
	if id != want {
		t.Fatalf("id = %d, want %d", id, want)
	}

	d := Decompose(id)
	if d["time"] != 100 || d["sequence"] != 3 || d["machine-id"] != 7 || d["msb"] != 0 {
		t.Fatalf("decompose = %v", d)
	}

	if got := Time(base, id); !got.Equal(base.Add(time.Second)) {
		t.Fatalf("time = %v, want %v", got, base.Add(time.Second))
	}
}

func TestComposeErrors(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sf, _ := New(Settings{StartTime: base, MachineID: fixedMachine(1)})

	if _, err := Compose(sf, base.Add(-time.Hour), 0, 1); err != ErrStartTimeAhead {
		t.Fatalf("want ErrStartTimeAhead, got %v", err)
	}
	if _, err := Compose(sf, base.Add(time.Hour), 1<<BitLenSequence, 1); err != ErrInvalidSequence {
		t.Fatalf("want ErrInvalidSequence, got %v", err)
	}
}

func TestNextIDUniqueAndMonotonic(t *testing.T) {
	base := time.Now().Add(-24 * time.Hour)
	sf, err := New(Settings{StartTime: base, MachineID: fixedMachine(9)})
	if err != nil {
		t.Fatal(err)
	}

	const n = 20000
	seen := make(map[uint64]struct{}, n)
	var last uint64
	for i := 0; i < n; i++ {
		id, err := sf.NextID()
		if err != nil {
			t.Fatalf("NextID: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = struct{}{}
		if id <= last {
			t.Fatalf("id not increasing: %d <= %d", id, last)
		}
		last = id
	}
}

func TestNextIDConcurrent(t *testing.T) {
	base := time.Now().Add(-24 * time.Hour)
	sf, err := New(Settings{StartTime: base, MachineID: fixedMachine(11)})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines, per = 16, 3000
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all = make(map[uint64]struct{}, goroutines*per)
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]uint64, 0, per)
			for j := 0; j < per; j++ {
				id, err := sf.NextID()
				if err != nil {
					t.Errorf("NextID: %v", err)
					return
				}
				local = append(local, id)
			}
			mu.Lock()
			for _, id := range local {
				if _, dup := all[id]; dup {
					t.Errorf("duplicate id %d", id)
				}
				all[id] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(all) != goroutines*per {
		t.Fatalf("unique = %d, want %d", len(all), goroutines*per)
	}
}

func TestBatchNextIDs(t *testing.T) {
	sf, err := New(Settings{StartTime: time.Now().Add(-time.Hour), MachineID: fixedMachine(5)})
	if err != nil {
		t.Fatal(err)
	}

	ids, err := sf.NextIDs(500)
	if err != nil {
		t.Fatalf("NextIDs: %v", err)
	}
	if len(ids) != 500 {
		t.Fatalf("len = %d, want 500", len(ids))
	}
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate in batch: %d", id)
		}
		seen[id] = struct{}{}
	}
	if _, err := sf.NextIDs(0); err != ErrInvalidBatchSize {
		t.Fatalf("want ErrInvalidBatchSize, got %v", err)
	}
}

// 时钟回拨：Logical 策略下不停服、不回退（sonyflake 语义）
func TestClockBackwardLogical(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	c := newClock(base)
	sf, err := New(Settings{
		StartTime: base.Add(-time.Hour),
		MachineID: fixedMachine(2),
		Now:       c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := sf.NextID()
	if err != nil {
		t.Fatal(err)
	}
	c.Set(base.Add(-time.Minute)) // 往回跳一分钟
	second, err := sf.NextID()
	if err != nil {
		t.Fatalf("logical policy should not fail: %v", err)
	}
	if second <= first {
		t.Fatalf("logical policy returned non-increasing id: %d <= %d", second, first)
	}
}

// 时钟回拨：Strict 策略直接报错
func TestClockBackwardStrict(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	c := newClock(base)
	sf, err := New(Settings{
		StartTime:       base.Add(-time.Hour),
		MachineID:       fixedMachine(3),
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
	c.Set(base.Add(-100 * time.Millisecond))
	if _, err := sf.NextID(); err != ErrClockBackward {
		t.Fatalf("want ErrClockBackward, got %v", err)
	}
}

// 时钟回拨：WaitRetry 在容忍窗口内等一下就能拿到号（Leaf 语义）
func TestClockBackwardWaitRetry(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	c := newClock(base)
	sf, err := New(Settings{
		StartTime:       base.Add(-time.Hour),
		MachineID:       fixedMachine(4),
		BackwardPolicy:  WaitRetry,
		MaxBackwardWait: 100 * time.Millisecond,
		Now:             c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sf.NextID(); err != nil {
		t.Fatal(err)
	}

	c.Set(base.Add(-20 * time.Millisecond)) // 回拨 20ms，在窗口内
	go func() {
		time.Sleep(30 * time.Millisecond)
		c.Set(base.Add(50 * time.Millisecond)) // 让时钟自己追上
	}()

	done := make(chan struct{})
	var (
		id   uint64
		err2 error
	)
	go func() {
		id, err2 = sf.NextID()
		close(done)
	}()
	select {
	case <-done:
		if err2 != nil {
			t.Fatalf("NextID after backward: %v", err2)
		}
		if id == 0 {
			t.Fatal("zero id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NextID blocked too long")
	}
}

func TestOverTimeLimit(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	c := newClock(base)
	sf, err := New(Settings{StartTime: base, MachineID: fixedMachine(1), Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	// 39 位时间片 × 10ms ≈ 174 年，跳到 200 年后才越界
	c.Set(base.Add(200 * 365 * 24 * time.Hour))
	if _, err := sf.NextID(); err != ErrOverTimeLimit {
		t.Fatalf("want ErrOverTimeLimit, got %v", err)
	}
}

func TestStartTimeAhead(t *testing.T) {
	base := time.Now().Add(time.Hour)
	if _, err := New(Settings{StartTime: base, MachineID: fixedMachine(1)}); err != ErrStartTimeAhead {
		t.Fatalf("want ErrStartTimeAhead, got %v", err)
	}
}

func TestCheckMachineID(t *testing.T) {
	sf, err := New(Settings{
		StartTime:      time.Now().Add(-time.Hour),
		MachineID:      fixedMachine(2),
		CheckMachineID: func(uint16) bool { return false },
	})
	if err != ErrInvalidMachineID {
		t.Fatalf("want ErrInvalidMachineID, got %v (sf=%v)", err, sf)
	}
}

func TestErrorCodes(t *testing.T) {
	if ErrorCode(ErrCacheNotInit) != -1 || ErrorCode(ErrKeyNotExists) != -2 || ErrorCode(ErrSegmentNotReady) != -3 {
		t.Fatal("leaf-compatible error codes mismatch")
	}
	if ErrorCode(nil) != 0 {
		t.Fatal("nil should map to 0")
	}
}
