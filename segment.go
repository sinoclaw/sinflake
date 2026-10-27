package sinflake

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// 号段模式默认参数，与美团 Leaf 对齐。
const (
	DefaultMaxStep         int64 = 1000000
	DefaultSegmentDuration       = 15 * time.Minute
	DefaultSyncInterval          = time.Minute
	doubleBuffer           int64 = 2
)

// SegmentOption 调整号段生成器行为。
type SegmentOption func(*SegmentGenerator)

// WithMaxStep 单个号段的最大步长（默认 1000000，与 Leaf 一致）。
func WithMaxStep(step int64) SegmentOption {
	return func(g *SegmentGenerator) {
		if step > 0 {
			g.maxStep = step
		}
	}
}

// WithSegmentDuration 号段使用时长阈值（默认 15 分钟，与 Leaf 一致）。
func WithSegmentDuration(d time.Duration) SegmentOption {
	return func(g *SegmentGenerator) {
		if d > 0 {
			g.segmentDuration = d
		}
	}
}

// WithSyncInterval tag 同步间隔（默认 60 秒）。
func WithSyncInterval(d time.Duration) SegmentOption {
	return func(g *SegmentGenerator) {
		if d > 0 {
			g.syncInterval = d
		}
	}
}

// WithSegmentNow 注入时钟，测试用。
func WithSegmentNow(fn func() time.Time) SegmentOption {
	return func(g *SegmentGenerator) {
		if fn != nil {
			g.now = fn
		}
	}
}

// WithSegmentLogger 注入 logger。
func WithSegmentLogger(l *slog.Logger) SegmentOption {
	return func(g *SegmentGenerator) { g.log = l }
}

// Segment 是一个号段区间 [value, max)。
type Segment struct {
	value atomic.Int64
	max   atomic.Int64
	step  atomic.Int64
}

// ID 返回已发到的位置。
func (s *Segment) ID() int64 { return s.value.Load() }

// Max 返回区间右界（不含）。
func (s *Segment) Max() int64 { return s.max.Load() }

// Step 返回号段步长。
func (s *Segment) Step() int64 { return s.step.Load() }

func (s *Segment) idle() float64 { return float64(s.max.Load() - s.value.Load()) }

type segmentBuffer struct {
	mu     sync.RWMutex
	key    string
	segs   [2]*Segment
	cur    int
	next   atomic.Bool // nextReady
	busy   atomic.Bool // running：已有后台协程在换段
	initOK atomic.Bool

	step            int64
	minStep         int64
	updateTimestamp int64 // 上一次回库领号的时间（毫秒）
}

func newSegmentBuffer(key string) *segmentBuffer {
	b := &segmentBuffer{key: key}
	for i := range b.segs {
		b.segs[i] = &Segment{}
	}
	return b
}

func (b *segmentBuffer) current() *Segment { return b.segs[b.cur] }
func (b *segmentBuffer) nextPos() int      { return (b.cur + 1) % 2 }
func (b *segmentBuffer) switchPos()        { b.cur = b.nextPos() }

// SegmentView 是监控页用的快照。
type SegmentView struct {
	Key             string `json:"key"`
	CurrentID       int64  `json:"current_id"`
	MaxID           int64  `json:"max_id"`
	Step            int64  `json:"step"`
	NextReady       bool   `json:"next_ready"`
	Updating        bool   `json:"updating"`
	Idle            int64  `json:"idle"`
	LastAllocMillis int64  `json:"last_alloc_millis"`
}

// SegmentGenerator 是号段模式的发号器（对齐美团 Leaf SegmentIDGenImpl）。
type SegmentGenerator struct {
	store Store
	log   *slog.Logger
	now   func() time.Time

	maxStep         int64
	segmentDuration time.Duration
	syncInterval    time.Duration

	mu    sync.RWMutex
	cache map[string]*segmentBuffer

	initOK    atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// NewSegmentGenerator 创建号段生成器；需再调用 Init 才会从库里装载号段。
func NewSegmentGenerator(store Store, opts ...SegmentOption) (*SegmentGenerator, error) {
	if store == nil {
		return nil, ErrNoMachineIDProvider // 复用同一族错误：没有后端
	}
	g := &SegmentGenerator{
		store:           store,
		log:             slog.Default(),
		now:             time.Now,
		maxStep:         DefaultMaxStep,
		segmentDuration: DefaultSegmentDuration,
		syncInterval:    DefaultSyncInterval,
		cache:           make(map[string]*segmentBuffer),
		done:            make(chan struct{}),
	}
	for _, o := range opts {
		o(g)
	}
	return g, nil
}

// Init 从存储装载所有 tag 并启动每分钟同步协程。
func (g *SegmentGenerator) Init(ctx context.Context) error {
	if err := g.updateCacheFromDB(ctx); err != nil {
		return err
	}
	g.initOK.Store(true)

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		t := time.NewTicker(g.syncInterval)
		defer t.Stop()
		for {
			select {
			case <-g.done:
				return
			case <-t.C:
				if err := g.updateCacheFromDB(ctx); err != nil {
					g.log.Warn("sinflake: sync tags from store failed", "err", err)
				}
			}
		}
	}()
	return nil
}

// Close 停止后台协程。
func (g *SegmentGenerator) Close() error {
	g.closeOnce.Do(func() { close(g.done) })
	g.wg.Wait()
	return nil
}

func (g *SegmentGenerator) updateCacheFromDB(ctx context.Context) error {
	tags, err := g.store.GetAllTags(ctx)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	exists := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		exists[t] = struct{}{}
		if _, ok := g.cache[t]; !ok {
			g.cache[t] = newSegmentBuffer(t)
			g.log.Info("sinflake: add tag to cache", "tag", t)
		}
	}
	for tag := range g.cache {
		if _, ok := exists[tag]; !ok {
			delete(g.cache, tag)
			g.log.Info("sinflake: remove tag from cache", "tag", tag)
		}
	}
	return nil
}

// NextID 取下一条 ID。错误码与 Leaf 对齐：
// 缓存未初始化 → ErrCacheNotInit；key 不存在 → ErrKeyNotExists；双 buffer 都空 → ErrSegmentNotReady。
func (g *SegmentGenerator) NextID(ctx context.Context, key string) (int64, error) {
	if !g.initOK.Load() {
		return 0, ErrCacheNotInit
	}
	g.mu.RLock()
	buf, ok := g.cache[key]
	g.mu.RUnlock()
	if !ok {
		return 0, ErrKeyNotExists
	}

	// 首次使用：同步装一次号段（对应 Leaf 的 !buffer.isInitOk() 分支）
	if !buf.initOK.Load() {
		buf.mu.Lock()
		if !buf.initOK.Load() {
			if err := g.updateSegmentFromDB(ctx, buf, buf.current(), false); err != nil {
				buf.mu.Unlock()
				g.log.Warn("sinflake: init buffer failed", "key", key, "err", err)
				return 0, err
			}
			buf.initOK.Store(true)
		}
		buf.mu.Unlock()
	}
	return g.idFromBuffer(ctx, buf)
}

// NextIDs 连续取 n 个 ID（对齐 Leaf 的 batch 接口）。
func (g *SegmentGenerator) NextIDs(ctx context.Context, key string, n int) ([]int64, error) {
	if n <= 0 {
		return nil, ErrInvalidBatchSize
	}
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id, err := g.NextID(ctx, key)
		if err != nil {
			return out, err
		}
		out = append(out, id)
	}
	return out, nil
}

func (g *SegmentGenerator) idFromBuffer(ctx context.Context, buf *segmentBuffer) (int64, error) {
	const threshold = 0.9

	for {
		buf.mu.RLock()
		seg := buf.current()
		step := seg.Step()
		// 用到 90% 就异步预取下一段（Leaf 的阈值），预取期间不阻塞发号
		need := !buf.next.Load() && !buf.busy.Load() && step > 0 &&
			seg.idle() < threshold*float64(step)
		v := seg.value.Add(1) - 1
		ok := v < seg.max.Load()
		buf.mu.RUnlock()

		if need {
			g.tryAsyncPrefetch(ctx, buf)
		}
		if ok {
			return v, nil
		}

		// 当前段耗尽：同步换段（比 Leaf 原版更稳，Leaf 这里直接返回异常码）
		if err := g.refill(ctx, buf); err != nil {
			return 0, err
		}
	}
}

// tryAsyncPrefetch 在后台装下一段，全过程持写锁，保证 buffer 字段无竞态。
func (g *SegmentGenerator) tryAsyncPrefetch(ctx context.Context, buf *segmentBuffer) {
	if !buf.busy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer buf.busy.Store(false)

		buf.mu.Lock()
		defer buf.mu.Unlock()
		if buf.next.Load() {
			return
		}
		next := buf.segs[buf.nextPos()]
		if err := g.updateSegmentFromDB(ctx, buf, next, true); err != nil {
			g.log.Warn("sinflake: prefetch segment failed", "key", buf.key, "err", err)
			return
		}
		buf.next.Store(true)
	}()
}

// refill 同步地换到下一段：已有备段就切，没有就当场回库领一段。
func (g *SegmentGenerator) refill(ctx context.Context, buf *segmentBuffer) error {
	buf.mu.Lock()
	defer buf.mu.Unlock()

	cur := buf.current()
	if cur.value.Load() <= cur.max.Load() {
		return nil // 别的协程已经换好段了
	}
	if buf.next.Load() {
		buf.switchPos()
		buf.next.Store(false)
		return nil
	}
	next := buf.segs[buf.nextPos()]
	if err := g.updateSegmentFromDB(ctx, buf, next, true); err != nil {
		g.log.Error("sinflake: both segments are not ready", "key", buf.key, "err", err)
		return ErrSegmentNotReady
	}
	buf.switchPos()
	return nil
}

// updateSegmentFromDB 向存储领一个新号段，并按 Leaf 的规则自适应调整 step：
// 15 分钟内用完 → step 翻倍（上限 maxStep）；30 分钟还没用完 → step 减半（下限 DB 里的 step）。
func (g *SegmentGenerator) updateSegmentFromDB(ctx context.Context, buf *segmentBuffer, seg *Segment, useCustomStep bool) error {
	nextStep := int64(0) // 0 表示用表里配置的 step
	if !buf.initOK.Load() || buf.updateTimestamp == 0 {
		// 首次装载：沿用库里的 step
		nextStep = 0
	} else {
		duration := g.now().UnixMilli() - buf.updateTimestamp
		step := buf.step
		switch {
		case duration < g.segmentDuration.Milliseconds():
			if step*2 <= g.maxStep {
				step *= 2
			}
		case duration < 2*g.segmentDuration.Milliseconds():
			// 保持
		default:
			if step/2 >= buf.minStep {
				step /= 2
			}
		}
		nextStep = step
	}

	alloc, err := g.store.UpdateMaxID(ctx, buf.key, nextStep)
	if err != nil {
		return err
	}
	step := buf.step
	if nextStep > 0 {
		step = nextStep
	} else {
		step = alloc.Step
	}
	if step <= 0 {
		return ErrKeyNotExists
	}

	// 先写 value 再写 max（Leaf 注释里强调的顺序）
	seg.value.Store(alloc.MaxID - step)
	seg.step.Store(step)
	seg.max.Store(alloc.MaxID)

	buf.step = step
	buf.minStep = alloc.Step
	buf.updateTimestamp = g.now().UnixMilli()
	_ = useCustomStep
	return nil
}

// Cache 返回当前所有号段的快照，给监控页用。
func (g *SegmentGenerator) Cache() []SegmentView {
	g.mu.RLock()
	defer g.mu.RUnlock()

	out := make([]SegmentView, 0, len(g.cache))
	for k, b := range g.cache {
		b.mu.RLock()
		seg := b.current()
		out = append(out, SegmentView{
			Key:             k,
			CurrentID:       seg.ID(),
			MaxID:           seg.Max(),
			Step:            seg.Step(),
			NextReady:       b.next.Load(),
			Updating:        b.busy.Load(),
			Idle:            seg.Max() - seg.ID(),
			LastAllocMillis: b.updateTimestamp,
		})
		b.mu.RUnlock()
	}
	return out
}

// Allocs 直接读存储里的号段配置（监控/运维用）。
func (g *SegmentGenerator) Allocs(ctx context.Context) ([]Alloc, error) {
	return g.store.GetAllAllocs(ctx)
}
