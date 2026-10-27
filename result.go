package sinflake

import (
	"context"
	"fmt"
)

// Status 与美团 Leaf 的 Result/Status 形状对齐：
// 取号统一返回 (ID, Status)，而不是只返回 error。
type Status int

const (
	// StatusSuccess 取号成功。
	StatusSuccess Status = iota
	// StatusException 取号异常，ID 位置上是 Leaf 风格的负数异常码。
	StatusException
)

// String 与 Leaf 的枚举名一致。
func (s Status) String() string {
	if s == StatusSuccess {
		return "SUCCESS"
	}
	return "EXCEPTION"
}

// Result 对齐 Leaf 的 com.sankuai.inf.leaf.common.Result。
type Result struct {
	ID     int64
	Status Status
}

// String 输出格式与 Leaf 的 toString 一致：Result{id=..., status=...}
func (r Result) String() string {
	return fmt.Sprintf("Result{id=%d, status=%s}", r.ID, r.Status)
}

// OK 表示取号成功。
func (r Result) OK() bool { return r.Status == StatusSuccess }

// OfID 把 (id, err) 折成 Result：失败时 ID 上放负数异常码。
func OfID(id int64, err error) Result {
	if err != nil {
		code := ErrorCode(err)
		if code == 0 {
			code = -1
		}
		return Result{ID: code, Status: StatusException}
	}
	return Result{ID: id, Status: StatusSuccess}
}

// Get 对齐 Leaf 的 IDGen#get(key)：号段取号，返回 Result。
func (g *SegmentGenerator) Get(ctx context.Context, key string) Result {
	id, err := g.NextID(ctx, key)
	return OfID(id, err)
}

// Get 对齐 Leaf 的 Snowflake 取号（key 不参与计算，仅保持接口一致）。
func (sf *Sinflake) Get(key string) Result {
	id, err := sf.NextID()
	return OfID(int64(id), err)
}
