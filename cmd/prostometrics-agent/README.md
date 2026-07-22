# Prostometrics Agent

Collection cadence:

- Startup: all enabled collectors run once immediately when the agent starts.
- Every 10s: CPU, memory, swap, network, disk I/O
- Every 60s: filesystem usage, inode counts, uptime

Metrics emitted by the agent (count metrics are deltas over the collection interval).
All metrics are sent with the configured workload scope; the table lists metric labels.

| Metric                           | Kind  | Unit    | Labels                                                          |
|----------------------------------|-------|---------|-----------------------------------------------------------------|
| `host.cpu.usage_pct`             | value | percent | `cpu`, `mode` (user,nice,system,idle,iowait,irq,softirq,steal)  |
| `host.mem.capacity_kb`           | value | KB      | `type` (total,used,free,available)                              |
| `host.swap.capacity_kb`          | value | KB      | `type` (total,used,free)                                        |
| `host.uptime_min`                | value | min     |                                                                 |
| `host.fs.capacity_kb`            | value | KB      | `mount`, `device`, `type` (total,used,free)                     |
| `host.fs.inodes_count`           | value | count   | `mount`, `device`, `type` (total,used,free)                     |
| `host.disk.io_kb`                | count | kb      | `device`, `dir` (read,write)                                    |
| `host.disk.io_ops`               | count | ops     | `device`, `dir` (read,write)                                    |
| `host.disk.io_time_ms`           | count | ms      | `device`                                                        |
| `host.net.kb`                    | count | kb      | `iface`, `dir` (rx,tx)                                          |
| `host.net.packets`               | count | packets | `iface`, `dir` (rx,tx)                                          |
| `host.net.errors`                | count | errors  | `iface`, `dir` (rx,tx)                                          |
| `host.net.dropped`               | count | packets | `iface`, `dir` (rx,tx)                                          |
| `docker.container.cpu.usage_pct` | value | percent | `service`                                                       |
| `docker.container.mem.usage_kb`  | value | kb      | `service`                                                       |
| `docker.container.net.kb`        | count | kb      | `service`, `dir` (rx,tx)                                        |
| `docker.container.restart_count` | count | count   | `service`                                                       |
| `nginx.connections`              | value | count   | `state` (active,reading,writing,waiting)                        |
| `nginx.totals`                   | count | count   | `type` (accepts,handled,requests)                               |
| `mongo.connections`              | value | count   | `instance`, `type` (current,available)                          |
| `mongo.mem.resident_mb`          | value | mb      | `instance`                                                      |
| `mongo.wt.cache.kb`              | value | kb      | `instance`, `type` (used,max)                                   |
| `mongo.wt.cache.evictions_count` | count | count   | `instance`                                                      |
| `mongo.ops_count`                | count | ops     | `instance`, `type` (insert,query,update,delete,getmore,command) |
| `mongo.op_latency_ms`            | value | ms      | `instance`, `type` (reads,writes,commands)                      |

Docker metrics are enabled automatically when a local Docker socket is detected at `/var/run/docker.sock`. Containers
are identified by their Compose service, Swarm service, or container name.

Nginx metrics require a reachable `stub_status` endpoint and are enabled by default unless explicitly disabled. You
can configure the endpoint explicitly or let the agent look for a local Nginx status page.

Mongo integration is enabled when instances are configured; the `enabled` flag is optional. Set `enabled: false` to
disable it even when instances are present.

Agent flags:

- `--workload` / `-w`: set the workload scope (defaults to hostname; empty value is an error).
- `--config` / `-c`: path to the YAML config file (optional).
- `--verbose` / `-v`: enable verbose client logging.

Logs:

- Foreground run: start with `--verbose` to see detailed logs in the terminal.
- For systemd install logs (`journalctl`), see `scripts/README.md`.

Environment overrides:

- `PROSTOMETRICS_API_KEY`: required API token for ingest auth.
- `PROSTOMETRICS_ENDPOINT`: full ingest URL (highest priority).
- `PROSTOMETRICS_HOST`: host or URL used to build the ingest endpoint.
- Environment endpoint settings override the config file.

Config file (optional):

- System: `/etc/prostometrics/agent.yaml`
- User: `$XDG_CONFIG_HOME/prostometrics/agent.yaml` (fallback: `~/.config/prostometrics/agent.yaml`)
- Override path via `--config` / `-c` or `PROSTOMETRICS_CONFIG`.
- When running as root, the system path is checked before the user path; otherwise user path is preferred.
- `${VAR}` expansion is supported for all string fields.
- `env_files` can provide `${VAR}` values from simple `KEY=VALUE` files (later files override earlier ones; process env
  is used as a fallback). Missing files are ignored. Optional `export ` prefix is supported. Values may be wrapped in
  single or double quotes (quotes are stripped).
- Config values override flags for overlapping fields (e.g., `agent.workload`); an empty workload is an error.

Example:

```yaml
agent:
  workload: "my-workload"
env_files:
  - "/etc/prostometrics/agent.env"

integrations:
  nginx:
    enabled: true
    endpoint: "http://127.0.0.1/stub_status"
  mongo:
    instances:
      - uri: "mongodb://monitor:${MONGO_PASSWORD}@localhost:27017/admin"
```

Mongo note: `serverStatus` runs against the `admin` database and requires appropriate permissions for the configured
user.
