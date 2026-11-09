# oci-splitwiseforynab

Go service that imports [Splitwise](https://splitwise.com) group expenses into [YNAB](https://ynab.com) for a single user. No UI, no database — YNAB's `import_id` deduplication makes re-syncs safe.

## How it works

`POST /sync` fetches expenses from a Splitwise group for the last `SYNC_DAYS` days, computes each expense's net balance for the configured user (paid minus owed share), and imports them into a YNAB account as cleared transactions. Expenses are keyed by `splitwise:<id>` so YNAB silently drops duplicates on subsequent syncs.

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `SPLITWISE_API_KEY` | yes | — | Splitwise personal API key |
| `YNAB_API_KEY` | yes | — | YNAB personal access token |
| `SPLITWISE_GROUP_ID` | yes | — | Splitwise group ID to sync from |
| `YNAB_BUDGET_ID` | yes | — | YNAB budget ID |
| `YNAB_ACCOUNT_ID` | yes | — | YNAB account ID to import into |
| `SYNC_DAYS` | no | `30` | How many days back to fetch expenses |
| `SYNC_INTERVAL` | no | — | Auto-sync interval (e.g. `1h`). Zero disables the loop; use `POST /sync` or an external scheduler instead |
| `PORT` | no | `8080` | HTTP listen port (overridden by `LISTEN_ADDR`) |
| `LISTEN_ADDR` | no | `:PORT` | Full listen address. Set to `127.0.0.1:8080` to restrict to loopback |

Get a Splitwise API key at <https://secure.splitwise.com/oauth_clients>. Get a YNAB personal access token at <https://app.ynab.com/settings/developer>.

## Endpoints

- `POST /sync` — run a sync; returns 204 on success
- `GET /healthz` — health check for Cloud Run

## Deployment

The image is published to `ghcr.io/pdeslaur/oci-splitwiseforynab` as a multi-arch manifest (amd64 + arm64).

**Homelab (Podman on Raspberry Pi):**

```
podman run -d --restart=always \
  -e SPLITWISE_API_KEY=... \
  -e YNAB_API_KEY=... \
  -e SPLITWISE_GROUP_ID=... \
  -e YNAB_BUDGET_ID=... \
  -e YNAB_ACCOUNT_ID=... \
  -e SYNC_INTERVAL=1h \
  -e LISTEN_ADDR=127.0.0.1:8080 \
  -p 127.0.0.1:8080:8080 \
  ghcr.io/pdeslaur/oci-splitwiseforynab:latest
```

**Cloud Run:** omit `SYNC_INTERVAL` and `LISTEN_ADDR`; pair with [Cloud Scheduler](https://cloud.google.com/scheduler) hitting `POST /sync` on a schedule.
