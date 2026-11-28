# alert-relay

Sends a rule engine's alerts to a [Matrix](https://matrix.org) room, through [matrix-relay](../matrix-relay/README.md): once when an alert fires, and once when it resolves. It takes them where Alertmanager's API does, `POST /api/v2/alerts`, so any engine that notifies Alertmanager can use it: [vmalert](https://docs.victoriametrics.com/victoriametrics/vmalert/), Prometheus, Loki's ruler. It runs beside the engine and matrix-relay, in their pod.

## How it works

- **Once per change.** An engine sends every firing alert again at each evaluation. alert-relay keys an alert by its labels and sends a message only when one starts firing or resolves; a repeat only moves its end.
- **Recorded once sent.** An alert is recorded as firing, or forgotten as resolved, only once its message is sent. It answers 502 when sending fails, and the engine's next evaluation sends the alert again, which retries it.
- **Kept across restarts.** What's firing is in a SQLite database in `STORE_DIR`. Neither its restart nor the engine's, which sends everything firing again, sends anything twice.
- **Expiry.** An engine gives a firing alert an end, `endsAt`, which it moves with each repeat. Every `SWEEP_INTERVAL`, alert-relay resolves the alerts whose end passed with no update, saying so: `RESOLVED (no update since …)`. That's an engine that stopped, or a resolution sent while alert-relay was down.
- **One-off events.** A rule counting events over a window, such as each SSH login, fires when one happens and resolves once it ages out of the window. Annotated `notify_resolved: "false"`, its resolution sends nothing.

Messages are sent as notices, `m.notice`, which clients show as a bot's:

```
FIRING HostLogsSilent: web01 sent no logs for 15 minutes
RESOLVED HostLogsSilent: web01 sent no logs for 15 minutes
FIRING SSHWithoutSecurityKey {host=web01, src=192.0.2.10, user=alice}
```

The alert's `summary` annotation follows its name, or, without one, its other labels but those the engine adds: `alertgroup`, and `stats_result`, the name of a LogsQL rule's stats result. A `description` annotation follows on the lines after.

## Trust

- Anything in its pod can post alerts: it listens on loopback, unauthenticated, as matrix-relay does, through which anything in the pod can already send. It refuses browsers' requests, which carry `Origin` or `Sec-Fetch-Site`, so that a web page can't post alerts, even with DNS rebinding.
- Its database holds the labels and annotations of the alerts firing, nothing secret. Losing it costs at most one more message per alert firing, so it needs no backup; restoring an old one would hold alerts as firing that aren't, until they expire.

## Using it with vmalert

```sh
vmalert -notifier.url=http://127.0.0.1:9093 ...
```

with a rule like:

```yaml
- alert: SSHWithoutSecurityKey
  expr: '"Accepted publickey" -"ssh2: ED25519-SK " | extract "for <user> from <src> port" | rename _HOSTNAME as host | stats by (host, user, src) count()'
  annotations:
    summary: "{{ $labels.user }} logged in to {{ $labels.host }} from {{ $labels.src }} without a security key"
    notify_resolved: "false"
```

## Configuration

| Variable | Default | |
|---|---|---|
| `SEND_URL` | | matrix-relay's `/send`, like `http://127.0.0.1:8090/send`. Required. |
| `ROOM` | | The room ID or alias alerts go to, which matrix-relay is in. Required. |
| `ADDR` | `127.0.0.1:9093` | Where it listens: Alertmanager's port. Keep it on loopback. |
| `STORE_DIR` | `/var/lib/alert-relay` | Its database, `alerts.db`. |
| `SWEEP_INTERVAL` | `1m` | How often it resolves expired alerts. |

Under systemd, with `Notify=true`, it tells systemd it's ready once it listens.

## Testing

```sh
go test ./internal/alertrelay ./cmd/alert-relay
```
