package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const defaultOpTimeout = 3 * time.Second

type Instance struct {
	DSN   string
	Label string
}

type instanceState struct {
	Instance
	db          *sql.DB
	nextAttempt time.Time
}

// Collector reports MySQL and MariaDB from the global status counters. Everything
// here is a counter the server keeps anyway, so the cost of reading it is two
// statements per instance and no load on the data itself.
type Collector struct {
	every      time.Duration
	retryEvery time.Duration
	opTimeout  time.Duration
	instances  []instanceState
}

func NewCollector(instances []Instance, every time.Duration, retryEvery time.Duration) *Collector {
	state := make([]instanceState, len(instances))
	for i, inst := range instances {
		state[i] = instanceState{Instance: inst}
	}
	if retryEvery <= 0 {
		retryEvery = time.Minute
	}
	return &Collector{
		every:      every,
		retryEvery: retryEvery,
		opTimeout:  defaultOpTimeout,
		instances:  state,
	}
}

func (c *Collector) ID() string { return "mysql" }

func (c *Collector) Every() time.Duration { return c.every }

func (c *Collector) Close(_ context.Context) error {
	for i := range c.instances {
		c.resetDB(&c.instances[i])
	}
	return nil
}

func (c *Collector) Collect(ctx context.Context) error {
	now := time.Now()
	for i := range c.instances {
		inst := &c.instances[i]
		if now.Before(inst.nextAttempt) {
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := c.collectInstance(ctx, inst); err != nil {
			log.Printf("mysql: instance %s: %v", inst.Label, err)
			c.resetDB(inst)
			inst.nextAttempt = now.Add(c.retryEvery)
		}
	}
	return nil
}

func (c *Collector) collectInstance(parent context.Context, inst *instanceState) error {
	ctx, cancel := context.WithTimeout(parent, c.opTimeout)
	defer cancel()

	db, err := c.ensureDB(inst)
	if err != nil {
		return err
	}

	status, err := readNameValueQuery(ctx, db, "SHOW GLOBAL STATUS")
	if err != nil {
		return fmt.Errorf("SHOW GLOBAL STATUS: %w", err)
	}
	variables, _ := readNameValueQuery(ctx, db, "SHOW GLOBAL VARIABLES")

	instanceLabel := prostometrics.Label("instance", inst.Label)

	emitValue := func(metric string, key string, source map[string]string, labels ...string) {
		if v, ok := numeric(source, key); ok {
			prostometrics.ValueSparse(metric, v, append([]string{instanceLabel}, labels...)...)
		}
	}
	emitTotal := func(metric string, key string, labels ...string) {
		if v, ok := numeric(status, key); ok {
			prostometrics.Total(metric, v, append([]string{instanceLabel}, labels...)...)
		}
	}

	emitValue("mysql.connections", "Threads_connected", status, prostometrics.Label("type", "connected"))
	emitValue("mysql.connections", "Threads_running", status, prostometrics.Label("type", "running"))
	emitValue("mysql.connections", "max_connections", variables, prostometrics.Label("type", "max"))

	emitTotal("mysql.queries_count", "Queries")
	emitTotal("mysql.slow_queries_count", "Slow_queries")

	// Read requests are answered from the buffer pool; reads went to disk. The
	// gap between them is the cache hit rate without needing a second metric.
	emitTotal("mysql.innodb_reads_count", "Innodb_buffer_pool_read_requests", prostometrics.Label("type", "buffer_pool"))
	emitTotal("mysql.innodb_reads_count", "Innodb_buffer_pool_reads", prostometrics.Label("type", "disk"))

	emitTotal("mysql.rows_count", "Innodb_rows_read", prostometrics.Label("op", "read"))
	emitTotal("mysql.rows_count", "Innodb_rows_inserted", prostometrics.Label("op", "inserted"))
	emitTotal("mysql.rows_count", "Innodb_rows_updated", prostometrics.Label("op", "updated"))
	emitTotal("mysql.rows_count", "Innodb_rows_deleted", prostometrics.Label("op", "deleted"))

	emitTotal("mysql.aborted_count", "Aborted_clients", prostometrics.Label("type", "clients"))
	emitTotal("mysql.aborted_count", "Aborted_connects", prostometrics.Label("type", "connects"))

	emitTotal("mysql.table_locks_waited_count", "Table_locks_waited")
	emitTotal("mysql.tmp_disk_tables_count", "Created_tmp_disk_tables")

	collectReplication(ctx, db, instanceLabel)

	return nil
}

// collectReplication reads replica status if this server is one. The statement
// was renamed in MySQL 8.0.22 and the column set differs between versions and
// forks, so both names are tried and columns are read by name.
func collectReplication(ctx context.Context, db *sql.DB, instanceLabel string) {
	var row map[string]string
	for _, statement := range []string{"SHOW REPLICA STATUS", "SHOW SLAVE STATUS"} {
		if found, err := readSingleRow(ctx, db, statement); err == nil && len(found) > 0 {
			row = found
			break
		}
	}
	if row == nil {
		return
	}

	for _, key := range []string{"Seconds_Behind_Source", "Seconds_Behind_Master"} {
		if v, ok := numeric(row, key); ok {
			if v < 0 {
				v = 0
			}
			prostometrics.ValueSparse("mysql.replica_lag_sec", v, instanceLabel)
			break
		}
	}

	running := 0.0
	ioRunning := firstOf(row, "Replica_IO_Running", "Slave_IO_Running")
	sqlRunning := firstOf(row, "Replica_SQL_Running", "Slave_SQL_Running")
	if strings.EqualFold(ioRunning, "Yes") && strings.EqualFold(sqlRunning, "Yes") {
		running = 1
	}
	prostometrics.ValueSparse("mysql.replica_running", running, instanceLabel)
}

func firstOf(row map[string]string, keys ...string) string {
	for _, key := range keys {
		if v, ok := row[key]; ok {
			return v
		}
	}
	return ""
}

// readNameValueQuery reads the two-column shape of SHOW GLOBAL STATUS.
func readNameValueQuery(ctx context.Context, db *sql.DB, statement string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string, 512)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, rows.Err()
}

// readSingleRow reads a one-row result whose columns are not known in advance.
func readSingleRow(ctx context.Context, db *sql.DB, statement string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, rows.Err()
	}

	cells := make([]sql.NullString, len(columns))
	targets := make([]any, len(columns))
	for i := range cells {
		targets[i] = &cells[i]
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, err
	}

	out := make(map[string]string, len(columns))
	for i, name := range columns {
		if cells[i].Valid {
			out[name] = cells[i].String
		}
	}
	return out, nil
}

func numeric(source map[string]string, key string) (float64, bool) {
	raw, ok := source[key]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (c *Collector) ensureDB(inst *instanceState) (*sql.DB, error) {
	if inst.db != nil {
		return inst.db, nil
	}
	db, err := sql.Open("mysql", inst.DSN)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection is enough for two status statements, and it keeps the agent
	// from occupying connection slots the application needs.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)
	inst.db = db
	return db, nil
}

func (c *Collector) resetDB(inst *instanceState) {
	if inst.db == nil {
		return
	}
	_ = inst.db.Close()
	inst.db = nil
}
