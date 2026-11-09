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
| `PORT` | no | `8080` | HTTP listen port |

Get a Splitwise API key at <https://secure.splitwise.com/oauth_clients>. Get a YNAB personal access token at <https://app.ynab.com/settings/developer>.

## Endpoints

- `POST /sync` — run a sync; returns 204 on success
- `GET /healthz` — health check for Cloud Run

## Deployment

The image is published to `ghcr.io/pdeslaur/oci-splitwiseforynab`. Pair with [Cloud Scheduler](https://cloud.google.com/scheduler) hitting `POST /sync` on a schedule (e.g. every hour).
