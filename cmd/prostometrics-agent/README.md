# Prostometrics Agent — collector reference

Collection cadence:

- Startup: all enabled collectors run once immediately when the agent starts.
- Every 10s: heartbeat, CPU, load, pressure, memory, swap, network, disk I/O, Docker, Nginx status and access log,
  MongoDB, PostgreSQL, MySQL, Redis
- Every 30s: RabbitMQ, Prometheus targets
- Every 60s: filesystem usage, inode counts, uptime, kernel counters, systemd units, commands

Counter metrics are cumulative totals; only the delta between readings travels on the wire, so a counter that does
not move costs nothing. A one-minute cadence therefore loses no events for a counter — it only changes how quickly
a change is noticed.

All metrics are sent with the configured workload scope; the tables list metric labels.

## Host

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `host.heartbeat` | count | count | |
| `host.cpu.usage_pct` | value | percent | `cpu`, `mode` (user,nice,system,idle,iowait,irq,softirq,steal) |
| `host.load_per_core` | value | count | `window` (1m,5m,15m) |
| `host.pressure_pct` | value | percent | `resource` (cpu,memory,io), `kind` (some,full) |
| `host.mem.capacity_kb` | value | KB | `type` (total,used,free,available) |
| `host.swap.capacity_kb` | value | KB | `type` (total,used,free) |
| `host.swap_io_pages` | count | pages | `dir` (in,out) |
| `host.oom_kills` | count | count | |
| `host.uptime_min` | value | min | |
| `host.fs.capacity_kb` | value | KB | `mount`, `device`, `type` (total,used,free) |
| `host.fs.inodes_count` | value | count | `mount`, `device`, `type` (total,used,free) |
| `host.disk.io_kb` | count | kb | `device`, `dir` (read,write) |
| `host.disk.io_ops` | count | ops | `device`, `dir` (read,write) |
| `host.disk.io_time_ms` | count | ms | `device` |
| `host.disk.io_latency_ms` | value | ms | `device` |
| `host.net.kb` | count | kb | `iface`, `dir` (rx,tx) |
| `host.net.packets` | count | packets | `iface`, `dir` (rx,tx) |
| `host.net.errors` | count | errors | `iface`, `dir` (rx,tx) |
| `host.net.dropped` | count | packets | `iface`, `dir` (rx,tx) |

`host.heartbeat` exists so that a host going away is observable at all. Every other metric may legitimately fall
silent — a disk is unmounted, an interface renamed, a container stopped — so none of them can tell "nothing to
report" apart from "nobody is reporting". It is a counter with exactly one series and no labels, which is what lets
the shipped alert read an empty bucket as a zero and fire on silence.

`host.pressure_pct` reports Linux PSI, which measures time spent waiting rather than utilisation. A disk can be
fully busy with nothing waiting on it, and a machine can be half idle while every request stalls; pressure is the
reading that separates those. It needs Linux 4.20+ built with `CONFIG_PSI`, and the collector is skipped otherwise.

## Docker

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `docker.containers_count` | value | count | `state` (running,exited,restarting,paused,created,dead,other) |
| `docker.container.up` | value | 1/0 | `service` |
| `docker.container.healthy` | value | 1/0 | `service` |
| `docker.container.oom_killed` | value | 1/0 | `service` |
| `docker.container.cpu.usage_pct` | value | percent | `service` |
| `docker.container.cpu.throttled_pct` | value | percent | `service` |
| `docker.container.mem.usage_kb` | value | kb | `service` |
| `docker.container.mem.limit_kb` | value | kb | `service` |
| `docker.container.mem.limit_used_pct` | value | percent | `service` |
| `docker.container.net.kb` | count | kb | `service`, `dir` (rx,tx) |
| `docker.container.restart_count` | count | count | `service` |

Docker metrics are enabled automatically when a local Docker socket is detected at `/var/run/docker.sock`. Containers
are identified by their Compose service, Swarm service, or container name.

`docker.container.up` is read from the full container list rather than from live statistics, so a container that
crashed and stayed down keeps reporting zero instead of vanishing — and a container that was genuinely removed stops
reporting altogether. That difference is what an alert needs.

The memory limit and the share of it are reported only when a limit was actually set. Docker hands an unconstrained
container the host's own memory as its limit, and reporting a share of that would show every such container at a
fixed, meaningless distance from being killed.

`docker.container.mem.limit_used_pct` is computed from the working set — reported usage minus the page cache the
kernel would drop under pressure — which is what the container is actually killed for.

## systemd

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `systemd.units_count` | value | count | `state` (failed,loaded) |
| `systemd.unit_failed` | value | 1/0 | `unit` |

Enabled when the host was booted by systemd (`/run/systemd/system` exists) and `systemctl` is runnable. At most 20
failing units are named individually; the count is always exact. A unit that recovers reports a zero once, so its
line returns to the floor instead of staying frozen at its last failing value.

## Nginx

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `nginx.connections` | value | count | `state` (active,reading,writing,waiting) |
| `nginx.totals` | count | count | `type` (accepts,handled,requests) |
| `nginx.requests_count` | count | count | `class` (1xx,2xx,3xx,4xx,5xx) |
| `nginx.request_time_ms` | value | ms | |
| `nginx.upstream_time_ms` | value | ms | |
| `nginx.pages` | top | visitors | |
| `nginx.error_pages` | top | visitors | |
| `nginx.referrers` | top | visitors | |

The status page requires a reachable `stub_status` endpoint and is enabled by default unless explicitly disabled. You
can configure the endpoint explicitly or let the agent look for a local status page.

Status classes and response times come from the access log instead, because `stub_status` counts connections rather
than outcomes: a site returning nothing but errors looks identical to a healthy one there. Reading is enabled by
default when `/var/log/nginx/access.log` is readable, follows the file across rotation and truncation, and starts at
the end of the file so a restart does not replay history.

The stock `combined` format carries no timing, so only status classes are reported from it. Response times appear
once the format is extended, in either of the two conventional ways — bare decimals appended after the quoted
fields, or named `rt=`/`urt=` values:

```nginx
log_format timed '$remote_addr - $remote_user [$time_local] "$request" '
                 '$status $body_bytes_sent "$http_referer" "$http_user_agent" '
                 '$request_time $upstream_response_time';
access_log /var/log/nginx/access.log timed;
```

Response times are sampled rather than sent one per request: `time_samples_per_tick` (default 20) bounds how many are
reported each round. Percentiles are computed by the product from those samples, so a busy site's cost stays flat
instead of growing with its traffic.

### Ranked lists

`top_lists: true` adds three lists, each ranking its rows by how many different visitors drew them rather than by how
many requests arrived, so one person reloading a page fifty times does not put it at the top:

- `nginx.pages` — the pages people read. Stylesheets, scripts, fonts, images, media and source maps are left out:
  every visit fetches the same few of them, so they would outrank every article on the site.
- `nginx.error_pages` — the pages that failed, each row named by the status it returned: `502 /orders/:id`. A page
  answering 404 to one visitor and 502 to another is two problems and reads as two rows. A missing script or image
  does appear here, unlike a working one: a broken asset is worth seeing and is not fetched by every visit anyway.
- `nginx.referrers` — the sites visitors arrived from, reduced to the site itself; a leading `www.` is dropped so one
  site is one row.

They are off by default because they are the only thing the agent sends whose volume follows the site's traffic, and
the service bills per accepted event. The counts and times above cost a fixed handful of events per tick however busy
the site is: a counter reported ten thousand times in one batch is summed into a single event before it leaves the
machine. A ranked list cannot be folded that way, because counting different visitors is impossible from a total. The
client does drop a repeat of the same visitor on the same row within one batch, so a person reloading a page costs one
event rather than ten — how much that collapses depends on how many events the installed client puts in a batch, which
is a property of the client release rather than of the agent. Plan on close to one event per request.

At most 5000 requests per tick are ranked. Past that the rest of the tick is not ranked at all, which bounds both the
bill and what the agent can push at the client in one round: the counts and times are reported first and the ranked
rows last, so a queue too full to take everything drops the ranked rows rather than the numbers a host already had.

On a quiet site that is nothing. On a site serving a hundred requests a second it is a few million events a day, which
is a real line on the bill and worth deciding deliberately. What the service stores does not grow with it — each list
holds a fixed number of rows however many pages a site has.

Three things shape what the rows say:

- **Visitor identity.** The client address is hashed on the host and never sent. Behind a CDN or a load balancer every
  request arrives from the proxy, so enable nginx's `ngx_http_realip_module` (`set_real_ip_from` plus `real_ip_header`)
  or every visitor will look like the same handful of people.
- **Page names.** A path segment that names one record becomes `:id`, so `/orders/41` and `/orders/9002` share a row.
  Numbers, dashed UUIDs and long runs of hex are recognised; slugs are deliberately left alone, because
  `/guides/how-to-instrument-a-service` is a page. Query strings are dropped.
- **Your own site.** Set `site_host` to the site's own address to keep it out of the referrer list. Without it, a
  visitor moving from one of your pages to the next counts as a referral and your own domain tops the list.
- **What counts as a failure.** `nginx.error_pages` leaves out the statuses that arrive from many addresses without
  the site being broken: 401 and 403, which an auth-gated app answers to every logged-out poll; 499, which nginx logs
  whenever a visitor closes a tab mid-load; and 404 on a page, which is overwhelmingly scanners working through a list
  of addresses that never existed. A 404 on a *file* is kept: a browser only asks for a file the page it just loaded
  named, so that one is a deployment that shipped half of itself.

The referrer list needs a format that carries `$http_referer`, which `combined` does. It is read from the first quoted
field after the request, and a field that is not a URL is ignored, so a custom format that puts something else there
reports nothing rather than reporting nonsense.

## PostgreSQL

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `postgres.connections` | value | count | `instance`, `state` (active,idle,idle_in_transaction,idle_in_transaction_aborted,max) |
| `postgres.transactions_count` | count | count | `instance`, `db`, `type` (commit,rollback) |
| `postgres.blocks_count` | count | blocks | `instance`, `db`, `type` (hit,read) |
| `postgres.tuples_count` | count | rows | `instance`, `db`, `op` (returned,fetched,inserted,updated,deleted) |
| `postgres.deadlocks_count` | count | count | `instance`, `db` |
| `postgres.temp_files_count` | count | count | `instance`, `db` |
| `postgres.db_size_kb` | value | kb | `instance`, `db` |
| `postgres.longest_transaction_sec` | value | sec | `instance` |
| `postgres.locks_waiting` | value | count | `instance` |
| `postgres.replica_lag_sec` | value | sec | `instance` |

The per-database breakdown is capped at 20 databases. Replication lag is reported only on a replica, where it is real
staleness; on a primary the same query has no meaning and would publish a permanent zero.

A read-only role is enough: `GRANT pg_monitor TO monitor;`

Unix-socket connections work too, in the form libpq uses: `postgres:///postgres?host=/var/run/postgresql`.

## MySQL and MariaDB

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `mysql.connections` | value | count | `instance`, `type` (connected,running,max) |
| `mysql.queries_count` | count | count | `instance` |
| `mysql.slow_queries_count` | count | count | `instance` |
| `mysql.innodb_reads_count` | count | count | `instance`, `type` (buffer_pool,disk) |
| `mysql.rows_count` | count | rows | `instance`, `op` (read,inserted,updated,deleted) |
| `mysql.aborted_count` | count | count | `instance`, `type` (clients,connects) |
| `mysql.table_locks_waited_count` | count | count | `instance` |
| `mysql.tmp_disk_tables_count` | count | count | `instance` |
| `mysql.replica_lag_sec` | value | sec | `instance` |
| `mysql.replica_running` | value | 1/0 | `instance` |

Everything comes from `SHOW GLOBAL STATUS` and `SHOW GLOBAL VARIABLES`, so the agent puts no load on the data itself.
Replica status is read through both `SHOW REPLICA STATUS` and the older `SHOW SLAVE STATUS`, by column name, because
the statement was renamed in MySQL 8.0.22 and the columns differ between versions and forks. The agent holds one
connection per instance so it does not occupy slots the application needs.

Grants: `GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'monitor'@'localhost';`

## Redis

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `redis.memory_kb` | value | kb | `instance`, `type` (used,peak,rss,max) |
| `redis.memory_used_pct` | value | percent | `instance` |
| `redis.clients` | value | count | `instance`, `type` (connected,blocked) |
| `redis.keyspace_count` | count | count | `instance`, `type` (hits,misses) |
| `redis.evicted_keys_count` | count | count | `instance` |
| `redis.expired_keys_count` | count | count | `instance` |
| `redis.commands_count` | count | count | `instance` |
| `redis.connections_count` | count | count | `instance`, `type` (received,rejected) |
| `redis.keys_count` | value | count | `instance` |
| `redis.unsaved_changes` | value | count | `instance` |
| `redis.last_save_age_sec` | value | sec | `instance` |
| `redis.replica_link_up` | value | 1/0 | `instance` |
| `redis.replica_lag_sec` | value | sec | `instance` |

Everything comes from one `INFO` call. `redis.memory_used_pct` is reported only when `maxmemory` is configured;
without a ceiling Redis grows until the host kills it, and a share of nothing is not a reading. Replication metrics
appear only on a replica.

URIs accept `redis://`, `rediss://` for TLS, a bare `host:port`, a bracketed IPv6 address with or without a port,
and a path to a unix socket (`/var/run/redis.sock` or `unix:///var/run/redis.sock`).

## RabbitMQ

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `rabbitmq.objects` | value | count | `instance`, `type` (connections,channels,queues,consumers) |
| `rabbitmq.messages` | value | count | `instance`, `state` (ready,unacknowledged) |
| `rabbitmq.messages_count` | count | count | `instance`, `type` (publish,deliver,ack,redeliver) |
| `rabbitmq.queue_messages` | value | count | `instance`, `queue`, `state` (ready,unacknowledged) |
| `rabbitmq.queue_consumers` | value | count | `instance`, `queue` |

Read through the management API, which must be enabled. The per-queue breakdown covers the 20 deepest queues; queue
names are often generated per consumer, and an unbounded label there would become an identifier rather than a
category. A queue that drains out of that list reports a zero so its line does not stay frozen at its last depth.

Credentials given in the URL are moved into an authorization header before the request is sent, so they never reach a
proxy or server access log.

## MongoDB

| Metric | Kind | Unit | Labels |
|---|---|---|---|
| `mongo.connections` | value | count | `instance`, `type` (current,available) |
| `mongo.mem.resident_mb` | value | mb | `instance` |
| `mongo.wt.cache.kb` | value | kb | `instance`, `type` (used,max) |
| `mongo.wt.cache.evictions_count` | count | count | `instance` |
| `mongo.ops_count` | count | ops | `instance`, `type` (insert,query,update,delete,getmore,command) |
| `mongo.op_latency_ms` | value | ms | `instance`, `type` (reads,writes,commands) |

`serverStatus` runs against the `admin` database and requires appropriate permissions for the configured user.

## Prometheus targets

Any endpoint that speaks the Prometheus text format can be forwarded, which covers Traefik, Caddy, HAProxy,
ClickHouse, `kafka_exporter`, `node_exporter` and an application's own metrics endpoint without waiting for a new
agent release.

```yaml
integrations:
  prometheus:
    every: "30s"
    targets:
      - name: traefik                  # becomes the `target` label; must be unique
        url: "http://127.0.0.1:8080/metrics"
        metrics: ["traefik_service_requests_total", "traefik_*"]
        labels: ["code", "method", "service"]
        max_series: 100
```

- `metrics` and `labels` are allowlists. A pattern may end in `*` to accept a prefix. An empty list allows everything,
  which is rarely what you want: exporters routinely expose thousands of series.
- `max_series` (default 100) caps what one target may publish, and the agent logs once when it stops there.
- `every` applies to the whole collector rather than to a single target. Targets are scraped one after another within
  a collection, each with its own share of the deadline, so one endpoint that hangs cannot starve the others.
- Counters are forwarded as cumulative totals; everything else as gauges. Metric names are kept as the exporter wrote
  them, so the endpoint's own documentation still applies.
- Histogram buckets are dropped. One histogram is dozens of series and would cost more than every host metric put
  together; the accompanying `_sum` and `_count` series are kept, which is enough to recover an average.

## Commands

Anything a team can already answer with a one-line script — the age of a backup, the size of a directory, a row
count, a licence expiry.

```yaml
integrations:
  exec:
    every: "60s"
    commands:
      - metric: "backup_age_hours"
        command: "/usr/local/bin/backup-age"
        args: ["--latest"]
        kind: "value"                  # value (default) or counter
        labels:
          job: "nightly"
        timeout: "10s"
```

The command prints one number per line, optionally followed by labels:

```
12
```

```
12 job=nightly target=/var/backups
4  job=weekly  target=/srv
```

At most 50 lines per command are published. A command that prints a negative number is reported as an error rather
than published, because ingest does not accept negative values. When a command prints a label that `labels:` already
sets, the configured value is kept — ingest rejects an event whose labels repeat a name, and it drops the whole
sample rather than the offending label. A command may take longer than the default collection
timeout — the collector asks the runner for the time its slowest command needs, bounded by its own interval.

## Agent flags

- `--workload` / `-w`: set the workload scope (defaults to hostname; empty value is an error).
- `--config` / `-c`: path to the YAML config file (optional).
- `--verbose` / `-v`: enable verbose client logging.

## Logs

- Foreground run: start with `--verbose` to see detailed logs in the terminal.
- Each integration logs one line at startup saying whether it was detected and why.
- For systemd install logs (`journalctl`), see `scripts/README.md`.

## Environment overrides

- `PROSTOMETRICS_API_KEY`: required API token for ingest auth.
- `PROSTOMETRICS_ENDPOINT`: full ingest URL (highest priority).
- `PROSTOMETRICS_HOST`: host or URL used to build the ingest endpoint.
- Environment endpoint settings override the config file.

## Config file

- System: `/etc/prostometrics/agent.yaml`
- User: `$XDG_CONFIG_HOME/prostometrics/agent.yaml` (fallback: `~/.config/prostometrics/agent.yaml`)
- Override path via `--config` / `-c` or `PROSTOMETRICS_CONFIG`.
- When running as root, the system path is checked before the user path; otherwise user path is preferred.
- `${VAR}` expansion is supported for all string fields, including inside lists and label maps.
- `env_files` can provide `${VAR}` values from simple `KEY=VALUE` files (later files override earlier ones; process env
  is used as a fallback). Missing files are ignored. Optional `export ` prefix is supported. Values may be wrapped in
  single or double quotes (quotes are stripped).
- Config values override flags for overlapping fields (e.g., `agent.workload`); an empty workload is an error.
- Every instance-based integration is off without instances and on with them. Setting `enabled: true` while
  configuring nothing is an error rather than a silent no-op; `enabled: false` disables one that is configured.
- An instance is labelled by the host it connects to unless `label` names it explicitly, which matters when two
  instances live behind the same address. A service reached over a unix socket is named after the socket instead, so
  `/var/run/redis.sock` becomes `redis` and `postgres:///app?host=/var/run/postgresql` becomes `postgresql`.
