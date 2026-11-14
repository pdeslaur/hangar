# cloudflared-svid

Runs a [Cloudflare tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) without a tunnel token to provision: it gets the tunnel's credentials from [cloudflare-sts](https://github.com/pdeslaur/cloudflare-sts) with the workload's [SPIFFE](https://spiffe.io) identity, then becomes cloudflared.

## How it works

- **Exchange.** It fetches a JWT-SVID for `BROKER_URL` from the Workload API and exchanges it there, with `PROFILE` as the scope, for the credentials of the profile's tunnel. The broker sets a new secret on the tunnel at each exchange: credentials from an earlier one no longer connect.
- **Retries.** While the Workload API has no SVID for it, such as before the agent attests a Pi that just rebooted, or the broker can't be reached or answers 502, 503 or 504, it retries, from a second apart up to 30 seconds. It exits on any other refusal.
- **cloudflared.** It writes the credentials to `CREDENTIALS_FILE`, `0600`, and execs `cloudflared tunnel --no-autoupdate --config CLOUDFLARED_CONFIG run --credentials-file CREDENTIALS_FILE <tunnel ID>`. When cloudflared exits, so does the container, and its restart exchanges again.

## Trust

- The credentials connect the tunnel until the next exchange: keep `CREDENTIALS_FILE` on a tmpfs only its container mounts.
- Anything that can fetch the workload's JWT-SVIDs can take the tunnel over, by exchanging one: the broker's policy decides which SPIFFE ID gets which tunnel.

## Configuration

| Variable | Default | |
|---|---|---|
| `BROKER_URL` | | cloudflare-sts's URL, like `https://cloudflare-sts.example.com`, and the audience of the JWT-SVID. Required. |
| `PROFILE` | | The broker's profile for the tunnel, like `spiffe://example.org/svc/app:tunnel`. Required. |
| `CREDENTIALS_FILE` | | Where to write the tunnel's credentials, on a tmpfs. Required. |
| `CLOUDFLARED_CONFIG` | | cloudflared's config file, with the tunnel's `ingress`. Its `tunnel` and `credentials-file` are the command line's. Required. |
| `CLOUDFLARED` | `/usr/local/bin/cloudflared` | The cloudflared binary. |
| `SPIFFE_ENDPOINT_SOCKET` | | The Workload API, like `unix:///run/spire/agent/api.sock`. |

## Image

`ghcr.io/pdeslaur/cloudflared-svid`, for `linux/amd64` and `linux/arm64`, tagged `latest` and with the short commit SHA, on Cloudflare's [`cloudflare/cloudflared`](https://hub.docker.com/r/cloudflare/cloudflared) image for cloudflared itself. See [Images](../../README.md#images) for how it's built and how to verify its signature.
