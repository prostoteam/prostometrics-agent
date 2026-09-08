package postgres

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const (
	defaultOpTimeout = 3 * time.Second

	// maxDatabases bounds the per-database breakdown. A server with more
	// databases than this is a hosting box rather than one team's database, and
	// the label would become an identifier instead of a category.
	maxDatabases = 20
)

type Instance struct {
	URI   string
	Label string
}

type instanceState struct {
	Instance
	conn        *pgx.Conn
	nextAttempt time.Time
}

// Collector reports PostgreSQL through the counters the server keeps itself.
// Between them they answer the three questions a team actually asks when the
// database is blamed: is it out of connections, is it reading from disk instead
// of cache, and is something holding a transaction open.
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

func (c *Collector) ID() string { return "postgres" }

func (c *Collector) Every() time.Duration { return c.every }

func (c *Collector) Close(ctx context.Context) error {
	for i := range c.instances {
		c.resetConn(ctx, &c.instances[i])
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
			log.Printf("postgres: instance %s: %v", inst.Label, err)
			c.resetConn(context.Background(), inst)
			inst.nextAttempt = now.Add(c.retryEvery)
		}
	}
	return nil
}

func (c *Collector) collectInstance(parent context.Context, inst *instanceState) error {
	ctx, cancel := context.WithTimeout(parent, c.opTimeout)
	defer cancel()

	conn, err := c.ensureConn(ctx, inst)
	if err != nil {
		return err
	}
	instanceLabel := prostometrics.Label("instance", inst.Label)

	if err := collectDatabaseStats(ctx, conn, instanceLabel); err != nil {
		return err
	}
	// The remaining readings are independent; one unavailable view — an older
	// server, a restricted role — must not cost the others.
	collectConnections(ctx, conn, instanceLabel)
	collectActivity(ctx, conn, instanceLabel)
	collectLocks(ctx, conn, instanceLabel)
	collectReplication(ctx, conn, instanceLabel)
	collectSizes(ctx, conn, instanceLabel)

	return nil
}

func collectDatabaseStats(ctx context.Context, conn *pgx.Conn, instanceLabel string) error {
	const query = `
		SELECT datname, xact_commit, xact_rollback, blks_read, blks_hit,
		       tup_returned, tup_fetched, tup_inserted, tup_updated, tup_deleted,
		       deadlocks, temp_files
		FROM pg_stat_database
		WHERE datname IS NOT NULL AND datname NOT LIKE 'template%'
		ORDER BY xact_commit + xact_rollback DESC
		LIMIT $1`

	rows, err := conn.Query(ctx, query, maxDatabases)
	if err != nil {
		return fmt.Errorf("pg_stat_database: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			name                                          string
			commit, rollback, blksRead, blksHit           int64
			returned, fetched, inserted, updated, deleted int64
			deadlocks, tempFiles                          int64
		)
		if err := rows.Scan(&name, &commit, &rollback, &blksRead, &blksHit,
			&returned, &fetched, &inserted, &updated, &deleted,
			&deadlocks, &tempFiles); err != nil {
			return fmt.Errorf("scan pg_stat_database: %w", err)
		}
		dbLabel := prostometrics.Label("db", name)

		emit := func(metric string, total int64, labels ...string) {
			if total < 0 {
				return
			}
			prostometrics.Total(metric, float64(total), append([]string{instanceLabel, dbLabel}, labels...)...)
		}
		emit("postgres.transactions_count", commit, prostometrics.Label("type", "commit"))
		emit("postgres.transactions_count", rollback, prostometrics.Label("type", "rollback"))
		// Read against hit is the cache ratio: reads climbing while hits stall is
		// the working set no longer fitting in memory.
		emit("postgres.blocks_count", blksRead, prostometrics.Label("type", "read"))
		emit("postgres.blocks_count", blksHit, prostometrics.Label("type", "hit"))
		emit("postgres.tuples_count", returned, prostometrics.Label("op", "returned"))
		emit("postgres.tuples_count", fetched, prostometrics.Label("op", "fetched"))
		emit("postgres.tuples_count", inserted, prostometrics.Label("op", "inserted"))
		emit("postgres.tuples_count", updated, prostometrics.Label("op", "updated"))
		emit("postgres.tuples_count", deleted, prostometrics.Label("op", "deleted"))
		emit("postgres.deadlocks_count", deadlocks)
		emit("postgres.temp_files_count", tempFiles)
	}
	return rows.Err()
}

// collectConnections reports the connection pool against its ceiling. Running out
// of connections looks like a total outage to every client at once, and it is the
// most common way a healthy PostgreSQL stops answering.
func collectConnections(ctx context.Context, conn *pgx.Conn, instanceLabel string) {
	const query = `
		SELECT COALESCE(state, 'unknown') AS state, count(*)
		FROM pg_stat_activity
		WHERE pid <> pg_backend_pid()
		GROUP BY 1`

	rows, err := conn.Query(ctx, query)
	if err != nil {
		// Publishing the seeded zeros here would report a database with no
		// connections rather than a database the agent cannot see, and the
		// shipped alert would read that as a recovery.
		return
	}
	defer rows.Close()

	states := map[string]int64{
		"active":                        0,
		"idle":                          0,
		"idle in transaction":           0,
		"idle in transaction (aborted)": 0,
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return
		}
		states[state] = count
	}
	// A read that stopped part way through describes some of the connections,
	// which is indistinguishable from a database that just went quiet.
	if rows.Err() != nil {
		return
	}

	for state, count := range states {
		prostometrics.ValueSparse("postgres.connections", float64(count),
			instanceLabel, prostometrics.Label("state", normalizeStateLabel(state)),
		)
	}

	var maxConnections int64
	if err := conn.QueryRow(ctx,
		`SELECT setting::bigint FROM pg_settings WHERE name = 'max_connections'`,
	).Scan(&maxConnections); err == nil && maxConnections > 0 {
		prostometrics.ValueSparse("postgres.connections", float64(maxConnections),
			instanceLabel, prostometrics.Label("state", "max"),
		)
	}
}

// collectActivity reports the oldest open transaction. One forgotten transaction
// blocks cleanup for the whole server, and nothing else here would show it.
func collectActivity(ctx context.Context, conn *pgx.Conn, instanceLabel string) {
	const query = `
		SELECT COALESCE(EXTRACT(EPOCH FROM max(now() - xact_start)), 0)
		FROM pg_stat_activity
		WHERE xact_start IS NOT NULL AND state <> 'idle'`

	var seconds float64
	if err := conn.QueryRow(ctx, query).Scan(&seconds); err != nil {
		return
	}
	if seconds < 0 {
		seconds = 0
	}
	prostometrics.ValueSparse("postgres.longest_transaction_sec", seconds, instanceLabel)
}

func collectLocks(ctx context.Context, conn *pgx.Conn, instanceLabel string) {
	var waiting int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
		return
	}
	prostometrics.ValueSparse("postgres.locks_waiting", float64(waiting), instanceLabel)
}

// collectReplication reports only on a replica, where lag is real staleness. On a
// primary the same query has no meaning and would publish a permanent zero.
func collectReplication(ctx context.Context, conn *pgx.Conn, instanceLabel string) {
	var inRecovery bool
	if err := conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil || !inRecovery {
		return
	}
	var seconds float64
	if err := conn.QueryRow(ctx,
		`SELECT COALESCE(EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()), 0)`,
	).Scan(&seconds); err != nil {
		return
	}
	if seconds < 0 {
		seconds = 0
	}
	prostometrics.ValueSparse("postgres.replica_lag_sec", seconds, instanceLabel)
}

func collectSizes(ctx context.Context, conn *pgx.Conn, instanceLabel string) {
	const query = `
		SELECT datname, pg_database_size(datname)
		FROM pg_database
		WHERE datistemplate = false AND datallowconn
		ORDER BY pg_database_size(datname) DESC
		LIMIT $1`

	rows, err := conn.Query(ctx, query, maxDatabases)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			return
		}
		if size < 0 {
			continue
		}
		prostometrics.ValueSparse("postgres.db_size_kb", float64(size)/1024.0,
			instanceLabel, prostometrics.Label("db", name),
		)
	}
}

// normalizeStateLabel keeps label values short and free of spaces.
func normalizeStateLabel(state string) string {
	switch state {
	case "idle in transaction":
		return "idle_in_transaction"
	case "idle in transaction (aborted)":
		return "idle_in_transaction_aborted"
	}
	return strings.ReplaceAll(strings.TrimSpace(state), " ", "_")
}

func (c *Collector) ensureConn(ctx context.Context, inst *instanceState) (*pgx.Conn, error) {
	if inst.conn != nil && !inst.conn.IsClosed() {
		return inst.conn, nil
	}
	conn, err := pgx.Connect(ctx, inst.URI)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	inst.conn = conn
	return conn, nil
}

func (c *Collector) resetConn(ctx context.Context, inst *instanceState) {
	if inst.conn == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = inst.conn.Close(closeCtx)
	inst.conn = nil
}
