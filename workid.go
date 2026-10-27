package sinflake

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"sync"
	"time"
)

// WorkIDProvider 负责给每个实例分配唯一机器号。
//
// 对齐美团 Leaf 的 SnowflakeZookeeperHolder 语义（注册 → 拿到 workerId → 心跳续约 →
// 失联后号可被回收），只是把注册中心从 ZooKeeper 换成数据库，少一个外部运行时。
type WorkIDProvider interface {
	// Acquire 申请机器号；已被别人占用且租约未过期时返回 ErrMachineIDBusy。
	Acquire(ctx context.Context) (uint16, error)
	// KeepAlive 续约自己占用的号。
	KeepAlive(ctx context.Context) error
	// Release 主动释放（进程优雅退出时调用）。
	Release(ctx context.Context) error
}

// IPProvider 是 sonyflake 的默认做法：取第一块私有网卡 IP 的低 16 位。
// 不需要任何外部依赖，但换 IP / 容器漂移会导致机器号变化，多实例场景不建议。
type IPProvider struct{}

// Acquire 实现 WorkIDProvider。
func (IPProvider) Acquire(context.Context) (uint16, error) {
	return lower16BitPrivateIP(defaultInterfaceAddrs)
}

// KeepAlive 实现 WorkIDProvider（无状态，直接成功）。
func (IPProvider) KeepAlive(context.Context) error { return nil }

// Release 实现 WorkIDProvider（无状态，直接成功）。
func (IPProvider) Release(context.Context) error { return nil }

// MySQLWorkIDProvider 用一张小表做机器号租约，替代 Leaf 的 ZooKeeper。
//
//	CREATE TABLE sinflake_workid (
//	  machine_id smallint unsigned NOT NULL,
//	  instance   varchar(128) NOT NULL,
//	  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
//	  PRIMARY KEY (machine_id),
//	  KEY idx_instance (instance)
//	) ENGINE=InnoDB;
//
// instance 建议用「应用名@主机名:端口」，租约过期后号会被别的实例接管。
type MySQLWorkIDProvider struct {
	db       *sql.DB
	table    string
	instance string
	ttl      time.Duration
	maxID    int

	mu  sync.Mutex
	own uint16
	got bool
}

// NewMySQLWorkIDProvider 创建基于 MySQL 的机器号分配器。
// ttl 为租约时长（默认 60 秒），maxID 为可用号上限（默认 65535，即 16 位上限）。
func NewMySQLWorkIDProvider(db *sql.DB, table, instance string, ttl time.Duration, maxID int) (*MySQLWorkIDProvider, error) {
	if db == nil {
		return nil, fmt.Errorf("sinflake: nil *sql.DB")
	}
	if instance == "" {
		return nil, fmt.Errorf("sinflake: empty instance name")
	}
	if table == "" {
		table = "sinflake_workid"
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	if maxID <= 0 {
		maxID = 1<<BitLenMachineID - 1
	}
	return &MySQLWorkIDProvider{db: db, table: table, instance: instance, ttl: ttl, maxID: maxID}, nil
}

// Acquire 实现 WorkIDProvider：先认领自己已有的号，再从 0 往上找空位/过期号。
func (p *MySQLWorkIDProvider) Acquire(ctx context.Context) (uint16, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.got {
		return p.own, nil
	}

	// 1) 之前注册过？直接续约复用
	var id int
	err := p.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT machine_id FROM %s WHERE instance = ? LIMIT 1", p.table),
		p.instance).Scan(&id)
	if err == nil {
		if err := p.touch(ctx, uint16(id)); err != nil {
			return 0, err
		}
		p.own, p.got = uint16(id), true
		return p.own, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	// 2) 从 0 往上抢空位
	for i := 0; i <= p.maxID; i++ {
		cand := uint16(i)
		res, err := p.db.ExecContext(ctx,
			fmt.Sprintf("INSERT IGNORE INTO %s (machine_id, instance) VALUES (?, ?)", p.table),
			cand, p.instance)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			p.own, p.got = cand, true
			return cand, nil
		}
		// 3) 被占：若对方租约已过期就接管
		res, err = p.db.ExecContext(ctx,
			fmt.Sprintf("UPDATE %s SET instance = ?, updated_at = CURRENT_TIMESTAMP "+
				"WHERE machine_id = ? AND updated_at < DATE_SUB(NOW(), INTERVAL ? SECOND)", p.table),
			p.instance, cand, int(p.ttl.Seconds()))
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			p.own, p.got = cand, true
			return cand, nil
		}
	}
	return 0, ErrMachineIDBusy
}

// KeepAlive 实现 WorkIDProvider：刷新自己那行的 updated_at。
func (p *MySQLWorkIDProvider) KeepAlive(ctx context.Context) error {
	p.mu.Lock()
	id, got := p.own, p.got
	p.mu.Unlock()
	if !got {
		return fmt.Errorf("sinflake: work id not acquired yet")
	}
	return p.touch(ctx, id)
}

// Release 实现 WorkIDProvider：把自己的号标记为可回收（时间戳退到很久以前）。
func (p *MySQLWorkIDProvider) Release(ctx context.Context) error {
	p.mu.Lock()
	id, got := p.own, p.got
	p.own, p.got = 0, false
	p.mu.Unlock()
	if !got {
		return nil
	}
	_, err := p.db.ExecContext(ctx,
		fmt.Sprintf("UPDATE %s SET updated_at = '1970-01-02 00:00:00' WHERE machine_id = ? AND instance = ?", p.table),
		id, p.instance)
	return err
}

// StartKeepAlive 起一个后台续约协程，返回 stop 函数。
func (p *MySQLWorkIDProvider) StartKeepAlive(interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = p.ttl / 3
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := p.KeepAlive(ctx); err != nil {
					// 续约失败不 panic：号仍能用，等下一轮
					continue
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

func (p *MySQLWorkIDProvider) touch(ctx context.Context, id uint16) error {
	// 注意：不能用 RowsAffected 判断归属——MySQL 在「值没变化」时返回 0 行，
	// 而 updated_at 是秒级精度，同一秒内续约必然 0 行，会误判成被别的实例占用。
	// 正确做法：直接查归属。
	var owner string
	err := p.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT instance FROM %s WHERE machine_id = ?", p.table), id).Scan(&owner)
	if err == sql.ErrNoRows {
		return ErrMachineIDBusy
	}
	if err != nil {
		return err
	}
	if owner != p.instance {
		return ErrMachineIDBusy
	}
	if _, err := p.db.ExecContext(ctx,
		fmt.Sprintf("UPDATE %s SET updated_at = CURRENT_TIMESTAMP WHERE machine_id = ?", p.table),
		id); err != nil {
		return err
	}
	return nil
}

// MachineIDFromProvider 把 WorkIDProvider 适配成 Settings.MachineID。
func MachineIDFromProvider(ctx context.Context, p WorkIDProvider) func() (uint16, error) {
	return func() (uint16, error) { return p.Acquire(ctx) }
}

// LocalInstanceName 生成默认实例名：应用名@主机名。
func LocalInstanceName(app string) string {
	host, err := net.LookupAddr("") // 拿到本机名，失败就退回 localhost
	name := "localhost"
	if err == nil && len(host) > 0 {
		name = host[0]
	} else if h, e := net.LookupCNAME("localhost"); e == nil {
		name = h
	}
	return fmt.Sprintf("%s@%s", app, name)
}
