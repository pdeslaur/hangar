# oci-spliitforynab

Go service that imports [Spliit Cloud](https://github.com/antonio-ivanovski/spliit-cloud) group expenses into [YNAB](https://ynab.com) for a single user. No UI, no database — YNAB's `import_id` deduplication makes re-syncs safe.

## How it works

`POST /sync` fetches expenses from a Spliit group for the last `SYNC_DAYS` days, computes each expense's net balance for the signed-in user (paid minus owed share), and imports them into a YNAB account as cleared transactions. Settlements are skipped. Expenses are keyed by `spliit:<id>` so YNAB silently drops duplicates on subsequent syncs.

Per-participant amounts are computed with a Go port of Spliit's split math (split modes, currency conversion, and its seeded remainder distribution), so they match the Spliit UI to the cent.

## Signing in to Spliit

Spliit's API only accepts OAuth tokens from the authorization code flow, so the service hosts that flow itself:

1. Open `<PUBLIC_URL>/login` in a browser. The service registers an OAuth client with Spliit and redirects you to Spliit's consent screen.
2. Sign in and approve read access to groups and expenses.
3. Spliit redirects back to `<PUBLIC_URL>/callback`, and the service stores the grant in `STATE_DIR/token.json`.

Spliit refresh tokens are single-use and last 30 days: every refresh returns a new one, and replaying an old one revokes the grant. The service writes each new refresh token to `STATE_DIR` before using it, so `STATE_DIR` must be persistent and owned by a single instance. Syncing at least monthly keeps the grant alive; if it lapses, sign in again at `/login`.

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `SPLIIT_URL` | yes | — | Spliit web origin, e.g. `https://spliit.example.com` |
| `SPLIIT_GROUP_ID` | yes | — | Spliit group ID to sync from (from the group's URL) |
| `YNAB_API_KEY` | yes | — | YNAB personal access token |
| `YNAB_BUDGET_ID` | yes | — | YNAB budget ID |
| `YNAB_ACCOUNT_ID` | yes | — | YNAB account ID to import into |
| `STATE_DIR` | no | `/data` | Directory holding the Spliit OAuth grant |
| `PUBLIC_URL` | no | `http://127.0.0.1:8080` | Origin the browser reaches this service at; `/callback` under it is the OAuth redirect URI |
| `SYNC_DAYS` | no | `30` | How many days back to fetch expenses |
| `SYNC_INTERVAL` | no | — | Auto-sync interval (e.g. `1h`). Zero disables the loop; use `POST /sync` or an external scheduler instead |
| `PORT` | no | `8080` | HTTP listen port (overridden by `LISTEN_ADDR`) |
| `LISTEN_ADDR` | no | `:PORT` | Full listen address. Set to `127.0.0.1:8080` to restrict to loopback |

Get a YNAB personal access token at <https://app.ynab.com/settings/developer>.

## Endpoints

- `GET /login` — start signing in to Spliit
- `GET /callback` — OAuth redirect target
- `POST /sync` — run a sync; returns 204 on success
- `GET /healthz` — health check

`/login` and `/sync` act on the shared Spliit grant, so put the service behind authentication (or keep it on loopback) wherever it's reachable by others.

## Deployment

The image is published to `ghcr.io/pdeslaur/oci-spliitforynab` as a multi-arch manifest (amd64 + arm64).

**Homelab (Podman on Raspberry Pi):**

```
podman run -d --restart=always \
  -e SPLIIT_URL=https://spliit.example.com \
  -e SPLIIT_GROUP_ID=... \
  -e YNAB_API_KEY=... \
  -e YNAB_BUDGET_ID=... \
  -e YNAB_ACCOUNT_ID=... \
  -e SYNC_INTERVAL=1h \
  -e LISTEN_ADDR=127.0.0.1:8080 \
  -v spliitforynab:/data \
  -p 127.0.0.1:8080:8080 \
  ghcr.io/pdeslaur/oci-spliitforynab:latest
```

Then open <http://127.0.0.1:8080/login> to sign in.
