package gormpool

import (
	"context"
	"errors"
	"github.com/ALiuGuanyan/godbpool"
	"github.com/ALiuGuanyan/godbpool/gormpool/sqls"
	"github.com/ALiuGuanyan/godbpool/gormpool/sqls/mssql"
	"github.com/ALiuGuanyan/godbpool/gormpool/sqls/my"
	"github.com/ALiuGuanyan/godbpool/gormpool/sqls/postgre"
	"github.com/ALiuGuanyan/godbpool/gormpool/sqls/sqlite"
	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
	"strings"
	"sync"
	"time"
)

var (
	// ErrGetFromClosedPool if try to get a Conn from the closed pool
	ErrGetFromClosedPool = errors.New("pool: get from closed pool")
	// ErrExceedingMaxWaitingDuration try to get a Conn from the pool but exceeding the max waiting time
	ErrExceedingMaxWaitingDuration = errors.New("pool: exceeding the maximum waiting duration")
	// ErrSQLType try to connect the SQL which does not support by the pool
	ErrSQLType = errors.New("pool: sql type does not support")
	// ErrKeepLTCapacity keepConn larger than the pool capacity
	ErrKeepLTCapacity = errors.New("pool: KeepConn larger than Capacity")
	// ErrCapacity provide a invalid capacity
	ErrCapacity = errors.New("pool: invalid capacity size")
	// ErrEmptyArgs DB arg is empty
	ErrEmptyArgs = errors.New("pool: args cannot be empty")
)

// Options is pool configuration
type Options struct {
	// DB type, e.g. MySQL, SQLite3...
	Type godbpool.SQLType

	// DB connection configuration
	Args interface{}

	// Conn key generate function
	KeyFunc func() string

	// how many idle conn to keep when there are no work to do
	// this field should smaller than Capacity
	KeepConn uint64

	// Maximum number of connections allocated by the pool at a given time.
	Capacity uint64

	// Maximum waiting duration to get a conn from the pool
	MaxWaitDuration time.Duration

	connector sqls.Connector
}

func (o *Options) validate() (err error) {
	switch o.Type {
	case godbpool.MySQL:
		o.connector = my.New(o.Args)
	case godbpool.PostgreSQL:
		o.connector = postgre.New(o.Args)
	case godbpool.SQLite3:
		o.connector = sqlite.New(o.Args)
	case godbpool.SQLServer:
		o.connector = mssql.New(o.Args)
	default:
		return ErrSQLType
	}

	if o.Args == nil {
		return ErrEmptyArgs
	}

	if o.Capacity == 0 {
		return ErrCapacity
	}

	if o.KeepConn > o.Capacity {
		return ErrKeepLTCapacity
	}

	return nil
}

// Pool is connection pool
type Pool struct {
	Type godbpool.SQLType

	Args interface{}

	keyFunc func() string

	connector sqls.Connector

	// how many idle conn to keep when there are no work to do
	// this field should smaller than Capacity
	keepConn uint64

	// Maximum number of connections allocated by the pool at a given time.
	capacity uint64

	maxWaitDuration time.Duration

	mu sync.Mutex // mu protects the following fields

	idleConn *conns // idle connections in this pool

	busyConn *conns // busy connections in this pool

	closed           bool          // set to true when the pool is closed.
	ch               chan struct{} // limits open connections when p.Wait is true
	currentWaitCount uint64        // current number of connections waited for.
	totalWaitCount   uint64        // total number of connections waited for.
	waitDuration     time.Duration // total time waited for new connections.

	// dropped get counter
	// if exceeding the max wait duration to get a connection
	// then droppedGetCount will increase by 1
	droppedGetCount uint64

	ctx context.Context
}

// NewPool will create a new pool
func NewPool(ctx context.Context, opts Options) (p *Pool, err error) {
	err = opts.validate()
	if err != nil {
		return nil, err
	}

	p = &Pool{
		Type:             opts.Type,
		Args:             opts.Args,
		keyFunc:          opts.KeyFunc,
		connector:        opts.connector,
		keepConn:         opts.KeepConn,
		capacity:         opts.Capacity,
		maxWaitDuration:  opts.MaxWaitDuration,
		mu:               sync.Mutex{},
		idleConn:         newConns(),
		busyConn:         newConns(),
		closed:           false,
		ch:               make(chan struct{}, opts.Capacity),
		currentWaitCount: 0,
		totalWaitCount:   0,
		waitDuration:     0,
		droppedGetCount:  0,
		ctx:              ctx,
	}

	if p.keepConn == 0 {
		err = p.checkArgs()
		if err != nil {
			return nil, err
		}
		return p, nil
	}

	for i := uint64(0); i < p.keepConn; i++ {
		err = p.initConn()
		if err != nil {
			return nil, err
		}
	}

	go func() {
		select {
		case <-p.ctx.Done():
			p.Close()
		default:
		}
	}()
	return p, nil
}

// called when do not know DBType and DBArgs are valid
func (p *Pool) initConn() error {
	var (
		key string
		db  *gorm.DB
		err error
	)

	db, err = p.connector.Open()
	if err != nil {
		return err
	}

	if p.keyFunc == nil {
		key = strings.ReplaceAll(uuid.New().String(), "-", "")
	} else {
		key = p.keyFunc()
	}

	conn := &Conn{
		DB:           db,
		Key:          key,
		Created:      time.Now(),
		Updated:      time.Now(),
		UsageCounter: 0,
	}
	p.ch <- struct{}{}
	p.idleConn.put(conn)
	return nil
}

// called when KeepConn is 0, aims to check the
// args passed can be used to make a valid connection
func (p *Pool) checkArgs() error {
	db, err := p.connector.Open()
	if err != nil {
		return err
	}
	db.Close()
	return nil
}

// Get a SQL connection from the pool
func (p *Pool) Get() (conn *Conn, err error) {
	select {
	case <-p.ctx.Done():
		p.mu.Lock()
		p.droppedGetCount++
		p.mu.Unlock()
		return nil, ErrGetFromClosedPool
	default:
		p.mu.Lock()
		if p.closed {
			p.droppedGetCount++
			p.mu.Unlock()
			return nil, ErrGetFromClosedPool
		}

		if p.idleConn.size > 0 {
			conn = p.get()
			<-p.ch
			p.mu.Unlock()
			return conn, nil
		}

		if p.busyConn.size < p.capacity {
			err = p.initConn()
			if err != nil {
				return nil, err
			}
			conn = p.get()
			<-p.ch
			p.mu.Unlock()
			return conn, nil
		}

		timer := time.NewTimer(p.maxWaitDuration)
		start := time.Now()
		p.currentWaitCount++
		p.totalWaitCount++
		p.mu.Unlock()
		select {
		case <-p.ctx.Done():
			p.mu.Unlock()
			return nil, ErrGetFromClosedPool
		case <-p.ch:
			p.mu.Lock()
			conn = p.get()
			p.waitDuration += time.Since(start)
			p.currentWaitCount--
			p.mu.Unlock()
			return conn, nil
		case <-timer.C:
			p.mu.Lock()
			p.waitDuration += time.Since(start)
			p.droppedGetCount++
			p.currentWaitCount--
			p.mu.Unlock()
			return nil, ErrExceedingMaxWaitingDuration
		}
	}
}

func (p *Pool) get() (conn *Conn) {
	conn = p.idleConn.get()
	p.busyConn.put(conn)
	return conn
}

// Put back a connection in the pool
func (p *Pool) Put(conn *Conn) {
	p.mu.Lock()
	p.busyConn.deleteByKey(conn.Key)

	if p.idleConn.size < p.keepConn && !p.closed {
		p.idleConn.put(conn)
		p.ch <- struct{}{}
	} else {
		conn.DB.Close()
		conn = nil
	}
	p.mu.Unlock()
}

// Close the pool
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	p.idleConn.close()
	p.mu.Unlock()
}

// Status shows the current pool status of the pool
func (p *Pool) Status() (ps PoolState) {
	p.mu.Lock()
	ps = PoolState{
		IdleConnsState: ConnsState{
			Size:  p.idleConn.size,
			Conns: p.idleConn.conns,
		},
		BusyConnsState: ConnsState{
			Size:  p.busyConn.size,
			Conns: p.busyConn.conns,
		},
		Capacity:             p.capacity,
		Closed:               p.closed,
		Size:                 p.busyConn.size + p.idleConn.size,
		TotalWaitingDuration: p.waitDuration,
		CurrentWaitCount:     p.currentWaitCount,
		TotalWaitCount:       p.totalWaitCount,
		DroppedGetCount:      p.droppedGetCount,
	}
	p.mu.Unlock()
	return ps
}

type conns struct {
	keys  []string
	conns map[string]*Conn
	size  uint64
}

// Conn is a struct wraps the internal DB connection
type Conn struct {
	DB               *gorm.DB
	Key              string
	Created, Updated time.Time
	UsageCounter     uint64
}

func newConns() *conns {
	return &conns{
		keys:  []string{},
		conns: map[string]*Conn{},
		size:  0,
	}
}

func (cs *conns) get() (conn *Conn) {
	key := cs.keys[0]
	cs.keys = cs.keys[1:]
	conn = cs.conns[key]
	delete(cs.conns, key)
	cs.size--
	conn.UsageCounter++
	conn.Updated = time.Now()
	return conn
}

func (cs *conns) put(conn *Conn) {
	cs.keys = append(cs.keys, conn.Key)
	cs.conns[conn.Key] = conn
	cs.size++
	conn.Updated = time.Now()
}

func (cs *conns) deleteByKey(key string) {
	keys := make([]string, cs.size-1)
	for _, val := range cs.keys {
		if val != key {
			keys = append(keys, val)
		} else {
			cs.conns[val].Updated = time.Now()
		}
	}
	cs.keys = keys
	cs.size--
	delete(cs.conns, key)
}

func (cs *conns) close() {
	for _, conn := range cs.conns {
		conn.DB.Close()
	}
	for _, key := range cs.keys {
		delete(cs.conns, key)
	}
}

// ConnsState shows Connections List state
type ConnsState struct {
	Size  uint64
	Conns map[string]*Conn
}

// PoolState shows pool state
type PoolState struct {
	IdleConnsState       ConnsState
	BusyConnsState       ConnsState
	Capacity             uint64
	Size                 uint64
	Closed               bool
	TotalWaitingDuration time.Duration
	CurrentWaitCount     uint64
	TotalWaitCount       uint64
	DroppedGetCount      uint64
}
