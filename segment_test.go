package sinflake

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordingStore 记录每次回库领号的步长，用来验证 Leaf 的 step 自适应规则。
type recordingStore struct {
	*MemStore
	mu    sync.Mutex
	steps []int64
}

func (r *recordingStore) UpdateMaxID(ctx context.Context, key string, step int64) (Alloc, error) {
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.mu.Unlock()
	return r.MemStore.UpdateMaxID(ctx, key, step)
}

func (r *recordingStore) history() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, len(r.steps))
	copy(out, r.steps)
	return out
}

func newRecStore(step int64) *recordingStore {
	return &recordingStore{
		MemStore: NewMemStore(Alloc{Key: "order", MaxID: 1, Step: step}),
	}
}

func TestSegmentBasicSequence(t *testing.T) {
	ctx := context.Background()
	rs := newRecStore(3) // 故意把步长设小，逼出换段
	g, err := NewSegmentGenerator(rs)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if _, err := g.NextID(ctx, "nope"); err != ErrKeyNotExists {
		t.Fatalf("want ErrKeyNotExists, got %v", err)
	}

	const n = 60
	prev := int64(-1)
	for i := 0; i < n; i++ {
		id, err := g.NextID(ctx, "order")
		if err != nil {
			t.Fatalf("NextID #%d: %v", i, err)
		}
		if id <= prev {
			t.Fatalf("id not increasing: %d <= %d", id, prev)
		}
		prev = id
	}
}

func TestSegmentNotInit(t *testing.T) {
	g, err := NewSegmentGenerator(newRecStore(10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.NextID(context.Background(), "order"); err != ErrCacheNotInit {
		t.Fatalf("want ErrCacheNotInit, got %v", err)
	}
}

func TestSegmentConcurrent(t *testing.T) {
	ctx := context.Background()
	g, err := NewSegmentGenerator(newRecStore(200))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	const goroutines, per = 12, 800
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all = make(map[int64]struct{}, goroutines*per)
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, 0, per)
			for j := 0; j < per; j++ {
				id, err := g.NextID(ctx, "order")
				if err != nil {
					t.Errorf("NextID: %v", err)
					return
				}
				local = append(local, id)
			}
			mu.Lock()
			for _, id := range local {
				if _, dup := all[id]; dup {
					t.Errorf("duplicate segment id %d", id)
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

// 15 分钟内用完 → 步长翻倍；长时间没动 → 步长减半（Leaf 的 SEGMENT_DURATION 规则）
func TestSegmentStepAdaptation(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	c := newClock(base)
	rs := newRecStore(100)
	g, err := NewSegmentGenerator(rs,
		WithSegmentNow(c.Now),
		WithSegmentDuration(time.Minute),
		WithMaxStep(1000),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	// 首次装载：走库里 step（传 0）
	if _, err := g.NextID(ctx, "order"); err != nil {
		t.Fatal(err)
	}
	// 快速把这一段的 100 个号用掉，逼出异步预取；时钟只前进 1 秒（< duration）
	for i := 0; i < 120; i++ {
		c.Set(base.Add(time.Duration(i) * time.Millisecond))
		if _, err := g.NextID(ctx, "order"); err != nil {
			t.Fatalf("#%d: %v", i, err)
		}
	}
	waitFor(t, 2*time.Second, func() bool {
		cache := g.Cache()
		return len(cache) == 1 && (cache[0].NextReady || cache[0].Step > 100)
	})

	steps := rs.history()
	var sawDoubling bool
	for _, s := range steps {
		if s == 200 {
			sawDoubling = true
		}
	}
	if !sawDoubling {
		t.Fatalf("expected a 200-step alloc (100→200), got %v", steps)
	}
}

func TestSegmentCacheView(t *testing.T) {
	ctx := context.Background()
	g, err := NewSegmentGenerator(newRecStore(50))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if _, err := g.NextID(ctx, "order"); err != nil {
		t.Fatal(err)
	}
	views := g.Cache()
	if len(views) != 1 {
		t.Fatalf("cache size = %d, want 1", len(views))
	}
	v := views[0]
	if v.Key != "order" || v.MaxID <= 0 || v.Step <= 0 || v.Idle <= 0 {
		t.Fatalf("bad view: %+v", v)
	}

	allocs, err := g.Allocs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 1 || allocs[0].Key != "order" {
		t.Fatalf("allocs = %+v", allocs)
	}
}

func TestSegmentTagsSync(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore(
		Alloc{Key: "a", MaxID: 1, Step: 10},
		Alloc{Key: "b", MaxID: 1, Step: 10},
	)
	g, err := NewSegmentGenerator(mem, WithSyncInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if len(g.Cache()) != 2 {
		t.Fatalf("want 2 tags, got %d", len(g.Cache()))
	}

	// 新增一个 tag，等同步协程拉进来
	mem.mu.Lock()
	mem.data["c"] = &Alloc{Key: "c", MaxID: 1, Step: 10}
	mem.mu.Unlock()
	waitFor(t, 2*time.Second, func() bool { return len(g.Cache()) == 3 })

	// 删掉一个，等它被踢出缓存
	mem.mu.Lock()
	delete(mem.data, "a")
	mem.mu.Unlock()
	waitFor(t, 2*time.Second, func() bool { return len(g.Cache()) == 2 })
}

func TestSegmentBatch(t *testing.T) {
	ctx := context.Background()
	g, err := NewSegmentGenerator(newRecStore(100))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	ids, err := g.NextIDs(ctx, "order", 250)
	if err != nil {
		t.Fatalf("NextIDs: %v", err)
	}
	if len(ids) != 250 {
		t.Fatalf("len = %d, want 250", len(ids))
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate batch id %d", id)
		}
		seen[id] = struct{}{}
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}
