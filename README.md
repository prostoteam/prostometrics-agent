# Prostometrics Agent

Host-metrics agent for Prostometrics. It collects Linux host metrics and optional Docker, Nginx, and MongoDB metrics.

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
  mongo:
    instances:
      - uri: "mongodb://monitor:${MONGO_PASSWORD}@localhost:27017/admin"
```

Docker metrics are enabled when `/var/run/docker.sock` is available. Nginx probing is enabled unless explicitly
disabled. MongoDB is enabled only when instances are configured.

## Metrics

| Metric | Kind | Labels |
|---|---|---|
| `host.cpu.usage_pct` | value | `cpu`, `mode` |
| `host.mem.capacity_kb` | value | `type` |
| `host.swap.capacity_kb` | value | `type` |
| `host.uptime_min` | value | |
| `host.fs.capacity_kb` | value | `mount`, `device`, `type` |
| `host.fs.inodes_count` | value | `mount`, `device`, `type` |
| `host.disk.io_kb` | counter | `device`, `dir` |
| `host.disk.io_ops` | counter | `device`, `dir` |
| `host.disk.io_time_ms` | counter | `device` |
| `host.net.kb` | counter | `iface`, `dir` |
| `host.net.packets` | counter | `iface`, `dir` |
| `host.net.errors` | counter | `iface`, `dir` |
| `host.net.dropped` | counter | `iface`, `dir` |
| `docker.container.*` | mixed | `service`, plus metric-specific labels |
| `nginx.connections` | value | `state` |
| `nginx.totals` | counter | `type` |
| `mongo.*` | mixed | `instance`, plus metric-specific labels |

See [the detailed collector reference](cmd/prostometrics-agent/README.md) for cadence, units, and integration behavior.
