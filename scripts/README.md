# Deploying prostometrics-agent on Ubuntu

Use the installer to set up a systemd service that starts on boot.

## One-line installer (system service)

```bash
curl -fsSL https://raw.githubusercontent.com/prostoteam/prostometrics-agent/main/scripts/install_agent.sh | sudo bash -s -- --workload my-workload --verbose
```

The installer prompts for `PROSTOMETRICS_API_KEY` and saves it to `/etc/prostometrics/agent.env` with mode `600`.

Non-interactive install is also supported by passing `PROSTOMETRICS_API_KEY` in the environment:

```bash
curl -fsSL https://raw.githubusercontent.com/prostoteam/prostometrics-agent/main/scripts/install_agent.sh -o /tmp/install_agent.sh
sudo PROSTOMETRICS_API_KEY='123_xxx' bash /tmp/install_agent.sh --workload my-workload --verbose
```

Check status:

```bash
sudo systemctl status prostometrics-agent
```

Tail logs:

```bash
sudo journalctl -u prostometrics-agent -f
```

User service (no sudo):

```bash
SYSTEMD_SCOPE=user ./install_agent.sh --workload my-workload --verbose
systemctl --user status prostometrics-agent
```

Tail logs (user service):

```bash
journalctl --user -u prostometrics-agent -f
```

Note: user services start on boot only if lingering is enabled (`loginctl enable-linger $USER`).

If `--workload` is omitted, the agent defaults to the system hostname; passing an empty workload value is an error.
