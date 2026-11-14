# smtp-relay

Sends email for an app that only speaks SMTP, like Gatus, without an SMTP password to provision. It runs beside the app, in its pod: the app sends to it without authentication, and it sends each message through [cloudflare-sts](https://github.com/pdeslaur/cloudflare-sts) with the pod's [SPIFFE](https://spiffe.io) identity.

Built on [go-smtp](https://github.com/emersion/go-smtp).

## How it works

- **Sending.** For each message, it fetches a JWT-SVID for `BROKER_URL` from the Workload API and posts the message there, to `POST /email` with `PROFILE` and the SMTP envelope: the sender and every recipient, so a `Bcc` is sent too. The broker checks them, and that the message's `From` is the sender, against the profile.
- **Replies.** It answers the app only once the broker has: `250` once it sent the message, `451` when retrying can help (no SVID yet, the broker unreachable, or answering 502, 503 or 504), `554` when the broker refuses it. It keeps no queue: the app's own retries and errors are what they are with any SMTP server. A failure can come after the broker sent to some recipients, which a retry sends to again.
- **Limits.** Those of the broker, which are Cloudflare's: 50 recipients and 25 MiB a message.

## Trust

Anything in its pod can send as `PROFILE`: it listens on loopback, without authentication or TLS. It offers no `AUTH`, so apps send without credentials. An HTTP request sent to it, such as a browser's, ends in errors that close the connection before its body is read as commands.

## Configuration

| Variable | Default | |
|---|---|---|
| `BROKER_URL` | | cloudflare-sts's URL, like `https://cloudflare-sts.example.com`, and the audience of the JWT-SVIDs. Required. |
| `PROFILE` | | The broker's profile it sends as, like `spiffe://example.org/svc/app:email`. Required. |
| `ADDR` | `127.0.0.1:2525` | Where it listens. Keep it on loopback. |
| `SPIFFE_ENDPOINT_SOCKET` | | The Workload API, like `unix:///run/spire/agent/api.sock`. |

## Image

`ghcr.io/pdeslaur/smtp-relay`, for `linux/amd64` and `linux/arm64`, tagged `latest` and with the short commit SHA. See [Images](../../README.md#images) for how it's built and how to verify its signature.
