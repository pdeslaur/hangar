# matrix-relay

Keeps a [Matrix](https://matrix.org) session with end-to-end encryption for an app that can't, like Home Assistant or Gatus. It runs beside the app, in its pod: the app sends messages to rooms through it, and receives the messages allowed users send. It logs in through [tuwunel-svid-sts](https://github.com/pdeslaur/tuwunel-svid-sts) with the pod's [SPIFFE](https://spiffe.io) identity, so it holds no password or token to provision.

Built on [mautrix-go](https://github.com/mautrix/go), with its olm in Go, so without cgo.

## How it works

- **Login.** It fetches a JWT-SVID for `STS_AUDIENCE` from the Workload API and exchanges it at `STS_URL` for an access token, on the device its store holds; the first time, on a device the homeserver creates. Once four fifths of the token's lifetime has passed, it logs in again on the same device, which keeps its keys, retrying every minute while that fails.
- **Encryption.** Its crypto store, in `STORE_DIR`, holds the device's keys and the rooms' keys. Their secrets are encrypted with a key generated the first time, `pickle.key`, beside the store, as mautrix's bridges do: it protects a copy of the database alone, not of the whole directory.
- **Cross-signing.** The first time, it generates cross-signing keys for its user, keeping them in its store. It publishes them whenever its user has other keys, or none, and signs its device with them, so that clients show its messages as its user's rather than an unverified device's. Replacing a user's keys, such as a client's from before the relay, or its own from a store it lost, needs user-interactive auth, which the user, without a password, can't complete: it replaces them through tuwunel-svid-sts's [`cross-signing-keys`](https://github.com/pdeslaur/tuwunel-svid-sts#replacing-cross-signing-keys). Clients that saw the earlier keys show that its user's identity changed.
- **Rooms.** It joins the rooms a user in `ALLOWED_USERS` invites it to, and declines other invites.
- **Sending.** It answers once it has synced. Before sending to a room it isn't sure is encrypted, it asks the homeserver: it never sends in the clear to an encrypted room.
- **Receiving.** It forwards the messages users in `ALLOWED_USERS` send, decrypted, to `INBOUND_URL`, if set. It skips messages from before it started, such as commands sent while it was down.

## Trust

- Anything in its pod can send as its user: it listens on loopback, unauthenticated.
- `STORE_DIR` holds keys that read the rooms it's in, its user's cross-signing keys, and the key that encrypts them: keep it in a volume only its pod reads. Losing it, the relay makes its user a new identity the next time it starts.
- `INBOUND_URL` receives messages decrypted: keep it in its pod too.
- The device's tokens end only when they expire: logging out would delete the device, and its keys. See tuwunel-svid-sts's [devices and rotating access tokens](https://github.com/pdeslaur/tuwunel-svid-sts#devices-and-rotating-access-tokens).

## API

Send a message to a room it's in, by ID or alias; `notice` sends an `m.notice`, which clients show as a bot's:

```sh
curl -X POST -d '{"room": "#home:example.org", "body": "The door is open", "notice": true}' http://127.0.0.1:8090/send
```

```json
{"event_id": "$..."}
```

It answers 503 until it has synced, 403 for a room it isn't in, and 400 for anything but a JSON object with a room and a body.

A message it forwards to `INBOUND_URL`:

```json
{"room_id": "!...", "event_id": "$...", "sender": "@phil:example.org", "msgtype": "m.text", "body": "Open the garage", "encrypted": true}
```

## Configuration

| Variable | Default | |
|---|---|---|
| `HOMESERVER_URL` | | The homeserver's client API. Required. |
| `STS_URL` | | tuwunel-svid-sts's URL, like `http://matrix:8080/` through the pod's mTLS client; it replaces cross-signing keys at `cross-signing-keys` under it. Required. |
| `STS_AUDIENCE` | | The audience tuwunel-svid-sts requires of JWT-SVIDs. Required. |
| `ALLOWED_USERS` | | The users whose invites it accepts and whose messages it forwards: `@phil:example.org,...`. Required. |
| `INBOUND_URL` | | Where it forwards messages; unset, it forwards none. |
| `STORE_DIR` | `/var/lib/matrix-relay` | Its crypto store and the key encrypting it. |
| `ADDR` | `127.0.0.1:8090` | Where it listens. Keep it on loopback. |
| `SPIFFE_ENDPOINT_SOCKET` | | The Workload API, like `unix:///run/spire/agent/api.sock`. |

Under systemd, with `Notify=true`, it tells systemd it's ready once it has synced.

## Testing

mautrix needs the `goolm` build tag for its olm in Go:

```sh
go test -tags goolm ./...
```

To also test against a real tuwunel, run with docker and an image: it checks logins, renewals, restarts, invites, cross-signing, replacing earlier cross-signing keys, and messages both ways in encrypted rooms.

```sh
TUWUNEL_IMAGE=ghcr.io/matrix-construct/tuwunel:latest go test -tags goolm ./...
```

## Image

`ghcr.io/pdeslaur/matrix-relay`, for `linux/amd64` and `linux/arm64`, tagged `latest` and with the short commit SHA. It's built with [ko](https://ko.build) on [`cgr.dev/chainguard/static`](https://images.chainguard.dev/directory/image/static/overview) by this repo's [build workflow](.github/workflows/build.yaml), and rebuilt daily to pick up base image fixes. Verify its signature with:

```sh
cosign verify ghcr.io/pdeslaur/matrix-relay:latest \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity https://github.com/pdeslaur/matrix-relay/.github/workflows/build.yaml@refs/heads/main
```
