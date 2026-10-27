package sinflake

import (
	"context"
	"time"
)

// Alloc 对应美团 Leaf 的 leaf_alloc 表一行。
type Alloc struct {
	Key         string    `json:"key"`
	MaxID       int64     `json:"max_id"`
	Step        int64     `json:"step"`
	Description string    `json:"description,omitempty"`
	UpdateTime  time.Time `json:"update_time,omitempty"`
}

// Store 是号段模式的存储后端。生产用 MySQLStore，测试用 MemStore，
// 也可以自己实现（Redis / PG / 自研配置中心都可以）。
type Store interface {
	// GetAllTags 返回库里所有 biz_tag，用于每分钟同步缓存。
	GetAllTags(ctx context.Context) ([]string, error)
	// GetAllAllocs 返回所有号段配置，用于监控页。
	GetAllAllocs(ctx context.Context) ([]Alloc, error)
	// UpdateMaxID 原子地把 max_id 增加 step，返回加完之后的行。
	// 返回的 MaxID 是「下一个可用区间」的上界（左开右闭）。
	UpdateMaxID(ctx context.Context, key string, step int64) (Alloc, error)
}
