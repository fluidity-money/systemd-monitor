# systemd-monitor

Small systemd outage monitor, based on ../docker-monitor. Polls explicitly selected system services every 30 seconds and sends one Better Stack incoming-webhook alert per service after five minutes of observed non-active state. Persists outage timers and notification status across restarts; retries unsuccessful deliveries on the next poll. No third-party Go dependencies.

## Build and install

Requires Go 1.24+ to build and Linux with systemd and `systemctl` on PATH to run. Run directly on the monitored host, not inside an ordinary Docker container.

```
cd /c/f/systemd-monitor
make test
make cli
sudo make install
sudo systemd-sysusers /usr/lib/sysusers.d/systemd-monitor.conf
sudo install -m 0600 systemd-monitor.env.example /etc/systemd-monitor.env
```

Edit /etc/systemd-monitor.env with your actual webhook URL and service names before enabling. Do not overwrite an existing configuration when upgrading. Then:

```
sudo systemctl daemon-reload
sudo systemctl enable --now systemd-monitor.service
sudo journalctl -u systemd-monitor.service -f
```

The unit runs as the dedicated static `systemd-monitor` user and group; systemd provides its writable persistent StateDirectory and manages its ownership. `systemd-sysusers` creates the account without a login shell. A static identity avoids system-bus connection failures seen with DynamicUser on some hosts. Read-only system-manager queries normally need no root privileges. Custom D-Bus policies may require access adjustments. The application inherits environment variables; it does not load dotenv files itself. Restart the monitor after changing configuration. Run only one instance per state file.

`make install` installs the sysusers definition but does not create accounts or restart services. `DESTDIR` stages all files without changing host accounts; `SYSUSERS_DIR` overrides the definition directory (default `/usr/lib/sysusers.d`). Run the account-creation command on the target host before starting the service.

### Upgrade from DynamicUser

After updating the source on the monitored host:

```
sudo make install
sudo systemd-sysusers /usr/lib/sysusers.d/systemd-monitor.conf
sudo systemctl daemon-reload
sudo systemctl restart systemd-monitor.service
sudo journalctl -u systemd-monitor.service -n 30 --no-pager
```

Keep the existing `/etc/systemd-monitor.env` and state file; do not reinstall the example environment or delete outage history. StateDirectory remains `systemd-monitor`. The monitor stays unprivileged and retains its existing sandbox protections. Successful polls are silent; verify that new `check failed` messages stop.

## Environment

| Variable | Required | Meaning |
| --- | --- | --- |
| STATE_FILE | Yes | Persistent JSON file; example uses /var/lib/systemd-monitor/state.json. Use a separate file from docker-monitor. |
| BETTERUPTIME_WEBHOOK_URL | Yes | Better Stack incoming-webhook HTTP(S) URL. |
| SYSTEMD_SERVICES | Yes | Comma/whitespace-separated service names. Bare names such as nginx become nginx.service; exact instances such as worker@one.service are supported. worker@, worker@.service, worker@* and worker@*.service all select loaded instances of worker. Other globs are rejected. |
| POLL_INTERVAL | No | Positive Go duration; default 30s. Alert threshold remains five minutes. |

## Outage behavior

- Template selectors (for example `SYSTEMD_SERVICES="worker@ nginx.service"`) refresh instances every poll using `systemctl list-units --all`, including loaded inactive/failed instances. Overlapping selectors are deduplicated. Zero matches produce no alert. Instances that systemd unloads disappear from wildcard monitoring and their saved outage state is cleared; use exact instance names when disappearance itself must alert. Never-started instances are not discovered merely because a template exists.
- Only listed services are monitored, whether enabled or disabled. This avoids alerts for the many deliberately inactive services on a normal host.
- Loaded services with ActiveState=active are healthy, including active/exited services. Failed, inactive, activating, deactivating, reloading, missing and masked services count as non-active. Long startups or reloads exceeding five minutes can therefore alert.
- Non-active state changes do not reset the outage timer. An observed active state clears it and permits a fresh alert on a later outage. Removing a service from configuration clears its saved entry on the next successful poll.
- Like docker-monitor, recovery clears local state but does NOT send a recovery/resolution webhook.
- Polling cannot detect brief failures between checks or a restart loop that appears active at every sample. This checks systemd state, not HTTP/application health. Oneshot/timer-driven services that normally become inactive should not be listed.
- A systemctl timeout, permission error or malformed response aborts that poll, logs an error (including systemctl's stderr on command failure) and preserves previous state. It is not treated as proof of a service outage. Each command has a 15-second timeout; webhook requests also have a 15-second timeout.
- Whole-host failure, network loss, or failure of the monitor itself cannot reliably send an outbound alert. Use an external uptime check/heartbeat for those cases.
- State is atomically replaced with mode 0600. A missing file starts fresh; corrupt state causes startup failure rather than silently forgetting notifications. Normal successful polls are silent.
- Delivery and state persistence cannot be atomic together: a crash after delivery but before saving can retry the same incident. Retries use the same incident ID for receiver-side deduplication. Outage IDs include hostname, service name and first-observed timestamp; use distinct hostnames across monitored hosts.

## Better Stack mapping

Same incident envelope and mapping as docker-monitor:

- Creation rule: incident.status contains alert
- Title: incident.title
- Cause: incident.description
- Alert ID: incident.id
- Metadata: incident.metadata (hostname, service_id, service_name, state, first_seen, duration_seconds)

Example incident ID: systemd-HOST-worker.service-TIMESTAMP. Status is alert; severity is high. Title identifies both the service and host.

## Verification

```
go test -race ./...
go vet ./...
go build -o systemd-monitor .
systemd-analyze verify systemd-monitor.service
```

Unit verification expects /usr/local/bin/systemd-monitor to be installed. Automated tests use simulated systemctl output and local HTTP servers; they never stop host services or contact a real webhook. Before relying on production alerts, verify read access to your selected services and perform a controlled outage test on a disposable service.
