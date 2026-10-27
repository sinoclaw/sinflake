package sinflake

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// MySQLStore 是号段模式的 MySQL 实现，表结构与美团 Leaf 兼容。
//
//	CREATE TABLE leaf_alloc (
//	  biz_tag     varchar(128) NOT NULL DEFAULT '',
//	  max_id      bigint(20)   NOT NULL DEFAULT 1,
//	  step        int(11)      NOT NULL,
//	  description varchar(256) DEFAULT NULL,
//	  update_time timestamp    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
//	  PRIMARY KEY (biz_tag)
//	) ENGINE=InnoDB;
//
// 注意：本包不绑定任何驱动，调用方自行 import（如 github.com/go-sql-driver/mysql）
// 并传入 *sql.DB，保持「纯标准库 + 零外部运行时」。
type MySQLStore struct {
	db    *sql.DB
	table string
}

// NewMySQLStore 创建 MySQL 存储，table 留空则用 leaf_alloc。
func NewMySQLStore(db *sql.DB, table string) (*MySQLStore, error) {
	if db == nil {
		return nil, fmt.Errorf("sinflake: nil *sql.DB")
	}
	if table == "" {
		table = "leaf_alloc"
	}
	return &MySQLStore{db: db, table: table}, nil
}

// GetAllTags 实现 Store。
func (s *MySQLStore) GetAllTags(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("SELECT biz_tag FROM %s", s.table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// GetAllAllocs 实现 Store。
func (s *MySQLStore) GetAllAllocs(ctx context.Context) ([]Alloc, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf("SELECT biz_tag, max_id, step, description, update_time FROM %s", s.table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Alloc
	for rows.Next() {
		var (
			a  Alloc
			d  sql.NullString
			ut sql.NullTime
		)
		if err := rows.Scan(&a.Key, &a.MaxID, &a.Step, &d, &ut); err != nil {
			return nil, err
		}
		a.Description = d.String
		if ut.Valid {
			a.UpdateTime = ut.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateMaxID 实现 Store：**在一个事务里** 先加锁再读回，否则两个实例会拿到重叠区间。
//
// Leaf 的 IDAllocDaoImpl 用的是 autocommit=false 的 SqlSession（UPDATE 后 SELECT 再 commit），
// 靠行锁把「加号段 + 读回」变成一个原子动作；这里同样必须显式开事务，只用一条 UPDATE
// 加一条 SELECT 会在并发下读到别人加过的 max_id，导致两个实例发同一批号。
//
// step > 0 时按指定步长领（Leaf 的 updateMaxIdByCustomStep）；
// step <= 0 时沿用表里配置的 step（Leaf 的 updateMaxId）。
func (s *MySQLStore) UpdateMaxID(ctx context.Context, key string, step int64) (Alloc, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Alloc{}, err
	}
	defer func() { _ = tx.Rollback() }() // 已 Commit 时是 no-op

	var (
		stmt string
		args []any
	)
	if step > 0 {
		stmt = fmt.Sprintf("UPDATE %s SET max_id = max_id + ? WHERE biz_tag = ?", s.table)
		args = []any{step, key}
	} else {
		stmt = fmt.Sprintf("UPDATE %s SET max_id = max_id + step WHERE biz_tag = ?", s.table)
		args = []any{key}
	}
	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		return Alloc{}, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return Alloc{}, ErrKeyNotExists
	}

	var (
		a  Alloc
		d  sql.NullString
		ut sql.NullTime
	)
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT biz_tag, max_id, step, description, update_time FROM %s WHERE biz_tag = ?", s.table),
		key).Scan(&a.Key, &a.MaxID, &a.Step, &d, &ut)
	if err != nil {
		return Alloc{}, err
	}
	a.Description = d.String
	if ut.Valid {
		a.UpdateTime = ut.Time
	}
	if err := tx.Commit(); err != nil {
		return Alloc{}, err
	}
	return a, nil
}

// MemStore 是内存版 Store，用于单机/测试；重启后号段会从头开始，别在生产用它发号。
// 并发安全（自带互斥锁）。
type MemStore struct {
	mu   sync.Mutex
	data map[string]*Alloc
	now  func() time.Time
}

// NewMemStore 创建内存 Store，rows 为初始号段配置。
func NewMemStore(rows ...Alloc) *MemStore {
	m := &MemStore{data: make(map[string]*Alloc), now: time.Now}
	for _, r := range rows {
		c := r
		if c.MaxID == 0 {
			c.MaxID = 1
		}
		m.data[r.Key] = &c
	}
	return m
}

// GetAllTags 实现 Store。
func (m *MemStore) GetAllTags(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out, nil
}

// GetAllAllocs 实现 Store。
func (m *MemStore) GetAllAllocs(context.Context) ([]Alloc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Alloc, 0, len(m.data))
	for _, v := range m.data {
		out = append(out, *v)
	}
	return out, nil
}

// UpdateMaxID 实现 Store。
func (m *MemStore) UpdateMaxID(_ context.Context, key string, step int64) (Alloc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data[key]
	if !ok {
		return Alloc{}, ErrKeyNotExists
	}
	if step <= 0 {
		step = a.Step // 与 MySQLStore 一致：<=0 表示沿用表里的 step
	}
	a.MaxID += step
	a.UpdateTime = m.now()
	return *a, nil
}
