package sinflake_test

import (
	"context"
	"fmt"
	"time"

	"github.com/sinoclaw/sinflake"
)

// 最小接入：snowflake 模式，机器号自己给。
func ExampleNew() {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sf, err := sinflake.New(sinflake.Settings{
		StartTime: epoch,
		MachineID: func() (uint16, error) { return 7, nil },
		// 固定时钟只为让示例输出可复现，生产留空即可
		Now: func() time.Time { return epoch.Add(time.Second) },
	})
	if err != nil {
		panic(err)
	}

	id, err := sf.NextID()
	if err != nil {
		panic(err)
	}
	fmt.Println(id)
	fmt.Println(sinflake.MachineID(id), sinflake.SequenceNumber(id), sinflake.ElapsedTime(id))
	fmt.Println(sinflake.Time(epoch, id).Format(time.RFC3339))
	// Output:
	// 1677721607
	// 7 0 1s
	// 2025-01-01T00:00:01Z
}

// 号段模式：不需要时钟，机器号也不用管。
func ExampleSegmentGenerator() {
	ctx := context.Background()

	// 生产换成 sinflake.NewMySQLStore(db, "leaf_alloc")
	store := sinflake.NewMemStore(sinflake.Alloc{Key: "order", MaxID: 1, Step: 3})

	gen, err := sinflake.NewSegmentGenerator(store)
	if err != nil {
		panic(err)
	}
	if err := gen.Init(ctx); err != nil {
		panic(err)
	}
	defer gen.Close()

	for i := 0; i < 3; i++ {
		id, err := gen.NextID(ctx, "order")
		if err != nil {
			panic(err)
		}
		fmt.Println(id)
	}

	// 对齐 Leaf 的 (id, status) 形状
	res := gen.Get(ctx, "order")
	fmt.Println(res, res.OK())

	// 错误也是 Leaf 的负数异常码
	bad := gen.Get(ctx, "不存在的key")
	fmt.Println(bad)
	// Output:
	// 1
	// 2
	// 3
	// Result{id=4, status=SUCCESS} true
	// Result{id=-2, status=EXCEPTION}
}

// 拆解与反拼，用于对账/排障。
func ExampleDecompose() {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sf, err := sinflake.New(sinflake.Settings{
		StartTime: epoch,
		MachineID: func() (uint16, error) { return 1, nil },
	})
	if err != nil {
		panic(err)
	}

	id, err := sinflake.Compose(sf, epoch.Add(2*time.Second), 5, 9)
	if err != nil {
		panic(err)
	}
	d := sinflake.Decompose(id)
	fmt.Println(d["time"], d["sequence"], d["machine-id"])
	// Output:
	// 200 5 9
}

// 机器号租约：多实例部署时防撞号（这里用内存 store 演示流程，生产接 MySQL）。
func ExampleMachineIDFromProvider() {
	ctx := context.Background()

	var p sinflake.WorkIDProvider = sinflake.IPProvider{} // 生产换成 MySQLWorkIDProvider
	mid, err := p.Acquire(ctx)
	if err != nil {
		fmt.Println("acquire failed:", err)
		return
	}
	fmt.Println("machine id ok:", mid >= 0)
	// Output:
	// machine id ok: true
}
