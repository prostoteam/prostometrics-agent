# Prostometrics Agent

Host-metrics agent for Prostometrics. It collects Linux host metrics and, when they are present, Docker, systemd,
Nginx, MongoDB, PostgreSQL, MySQL, Redis and RabbitMQ metrics. Two generic collectors cover everything else: any
endpoint that speaks the Prometheus text format, and any command that prints a number.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/prostoteam/prostometrics-agent/main/scripts/install_agent.sh | sudo bash
```

The installer prompts for `PROSTOMETRICS_API_KEY`, installs `prostometrics-agent`, creates a systemd service, and writes
the key to `/etc/prostometrics/agent.env` with mode `0600`.

For a non-interactive installation:

```bash
curl -fsSL https://raw.githubusercontent.com/prostoteam/prostometrics-agent/main/scripts/install_agent.sh -o /tmp/prostometrics-install.sh
sudo PROSTOMETRICS_API_KEY='your-api-key' bash /tmp/prostometrics-install.sh
```

## What is enabled automatically

Nothing needs configuring for the host itself. Each optional integration is probed once at startup and skipped
quietly when it is not there, so an unsupported kernel or a missing service costs one log line rather than a
repeating failure.

| Integration | Enabled when |
|---|---|
| Core host metrics | always |
| Pressure and kernel metrics | the kernel exposes them (Linux; pressure needs 4.20+ with `CONFIG_PSI`) |
| Docker | `/var/run/docker.sock` is a reachable socket |
| systemd | the host was booted by systemd and `systemctl` is runnable |
| Nginx status | a `stub_status` endpoint answers |
| Nginx access log | `/var/log/nginx/access.log` is readable |
| Nginx ranked lists | `top_lists` is switched on for nginx |
| MongoDB, PostgreSQL, MySQL, Redis, RabbitMQ | instances are configured |
| Prometheus scrape, commands | targets are configured |

## Runtime configuration

- `PROSTOMETRICS_API_KEY`: required ingest API key.
- `PROSTOMETRICS_ENDPOINT`: complete ingest endpoint.
- `PROSTOMETRICS_HOST`: host or base URL used when the complete endpoint is absent.
- `PROSTOMETRICS_CONFIG`: optional YAML configuration path.
- `--workload` / `-w`: workload scope; defaults to the hostname.
- `--config` / `-c`: optional YAML configuration path.
- `--verbose` / `-v`: verbose SDK delivery logs.

Default configuration paths are `/etc/prostometrics/agent.yaml` for system installs and
`$XDG_CONFIG_HOME/prostometrics/agent.yaml` for user installs.

```yaml
agent:
  workload: "my-host"
env_files:
  - "/etc/prostometrics/agent.env"

integrations:
  nginx:
    enabled: true
    endpoint: "http://127.0.0.1/stub_status"
    access_log: "/var/log/nginx/access.log"
    # Ranked lists of pages, failing pages and referring sites. Off by default:
    # see the collector reference for what they cost.
    top_lists: true
    site_host: "example.com"

  postgres:
    instances:
      - uri: "postgres://monitor:${PG_PASSWORD}@localhost:5432/postgres"

  redis:
    instances:
      - uri: "redis://:${REDIS_PASSWORD}@localhost:6379"

  mysql:
    instances:
      - dsn: "monitor:${MYSQL_PASSWORD}@tcp(127.0.0.1:3306)/"

  rabbitmq:
    instances:
      - url: "http://monitor:${RABBIT_PASSWORD}@127.0.0.1:15672"

  mongo:
    instances:
      - uri: "mongodb://monitor:${MONGO_PASSWORD}@localhost:27017/admin"

  prometheus:
    targets:
      - name: traefik
        url: "http://127.0.0.1:8080/metrics"
        metrics: ["traefik_service_requests_total", "traefik_service_open_connections"]

  exec:
    commands:
      - metric: "backup_age_hours"
        command: "/usr/local/bin/backup-age"
```

## Metrics

| Metric | Kind | Labels |
|---|---|---|
| `host.heartbeat` | counter | |
| `host.cpu.usage_pct` | value | `cpu`, `mode` |
| `host.load_per_core` | value | `window` |
| `host.pressure_pct` | value | `resource`, `kind` |
| `host.mem.capacity_kb`, `host.swap.capacity_kb` | value | `type` |
| `host.swap_io_pages` | counter | `dir` |
| `host.oom_kills` | counter | |
| `host.uptime_min` | value | |
| `host.fs.capacity_kb`, `host.fs.inodes_count` | value | `mount`, `device`, `type` |
| `host.disk.io_kb`, `host.disk.io_ops` | counter | `device`, `dir` |
| `host.disk.io_time_ms` | counter | `device` |
| `host.disk.io_latency_ms` | value | `device` |
| `host.net.kb`, `host.net.packets`, `host.net.errors`, `host.net.dropped` | counter | `iface`, `dir` |
| `docker.containers_count` | value | `state` |
| `docker.container.*` | mixed | `service`, plus metric-specific labels |
| `systemd.units_count` | value | `state` |
| `systemd.unit_failed` | value | `unit` |
| `nginx.connections` | value | `state` |
| `nginx.totals` | counter | `type` |
| `nginx.requests_count` | counter | `class` |
| `nginx.request_time_ms`, `nginx.upstream_time_ms` | value | |
| `nginx.pages`, `nginx.error_pages`, `nginx.referrers` | top | |
| `postgres.*`, `mysql.*`, `redis.*`, `mongo.*` | mixed | `instance`, plus metric-specific labels |
| `rabbitmq.*` | mixed | `instance`, plus `queue`, `type` or `state` |

See [the detailed collector reference](cmd/prostometrics-agent/README.md) for cadence, units, integration behavior,
and the full configuration reference.
