# tuwunel-svid-sts

Exchanges [SPIFFE](https://spiffe.io) JWT-SVIDs for Matrix access tokens, so that bots log in to a [tuwunel](https://github.com/matrix-construct/tuwunel) homeserver by workload identity instead of a stored password or token.

It runs in the same pod as tuwunel. At startup it generates an ES256 key, kept only in memory, and writes a tuwunel config file trusting its public key for [JWT login](https://matrix-construct.github.io/tuwunel/authentication/jwt.html). Then, for each request:

1. it validates the JWT-SVID in the `Authorization: Bearer` header against the Workload API's JWT bundles, which follow the SPIRE server's key rotation, requiring `AUDIENCE`;
2. it maps the SVID's SPIFFE ID to a Matrix localpart through `USERS`, refusing IDs it doesn't list;
3. it signs a one-minute JWT for that localpart and logs in to tuwunel with it, as the device `TUWUNELSVIDSTS`;
4. it returns tuwunel's login response, holding the access token.

This works around two limits of tuwunel's JWT login: it takes a single static key, where SPIRE rotates its keys, and it uses `sub` as the localpart, where a JWT-SVID's `sub` is a SPIFFE ID.

## Trust

tuwunel accepts any `sub` its key signs, so the key can log in as any existing user, not only the ones in `USERS`. That's why it lives in tuwunel's pod and nowhere else: whoever compromises it could equally compromise tuwunel, which holds every access token. tuwunel only signs in existing users (`register_user = false`).

A JWT-SVID is a bearer token: until it expires, whoever holds it can exchange it for an access token lasting much longer, and, since every login uses the same device, end the session of the bot it belongs to. So `AUDIENCE` must be unique to this service, never accepted by another, so that no other service receiving an SVID can replay it here. Callers should fetch SVIDs with short lifetimes, just before exchanging them, and never log them.

## tuwunel config

The file written to `TUWUNEL_CONFIG_FILE`, for tuwunel to read through `TUWUNEL_CONFIG`; `TUWUNEL_` environment variables still override it:

```toml
[global.jwt]
enable = true
format = "ECDSA"
algorithm = "ES256"
key = """
-----BEGIN PUBLIC KEY-----
...
-----END PUBLIC KEY-----
"""
issuer = ["tuwunel-svid-sts"]
audience = ["tuwunel"]
require_exp = true
require_nbf = true
register_user = false
expire_access_token = true
```

A new key is generated each time the process starts, so tuwunel must start after it, and restart with it.

With `expire_access_token`, the access tokens it hands out last tuwunel's `access_token_ttl`, a week by default, without a refresh token: a bot logs in again when its token expires.

## Configuration

| Variable | Default | |
|---|---|---|
| `AUDIENCE` | | The audience a JWT-SVID needs, unique to this service. Required. |
| `USERS` | | SPIFFE IDs and the localparts they log in as: `spiffe://example.org/bot=bot,...`. Required. |
| `TUWUNEL_CONFIG_FILE` | | Where to write the tuwunel config file. Required. |
| `MATRIX_URL` | `http://localhost:6167` | tuwunel's client API. |
| `ADDR` | `127.0.0.1:8080` | Where it listens, plain HTTP for the mTLS sidecar in its pod. Keep it on loopback: on another interface, callers can skip the sidecar and send SVIDs and access tokens in the clear. |
| `SPIFFE_ENDPOINT_SOCKET` | | The Workload API, like `unix:///run/spire/agent/api.sock`. |

## Usage

With `$SVID` a JWT-SVID for the audience `AUDIENCE`, fetched from the caller's Workload API. The caller reaches it over mTLS: its own mTLS client, listening on loopback, connects to the sidecar in front of this service, so the request itself is plain HTTP to that client:

```sh
curl -X POST -H "Authorization: Bearer $SVID" http://tuwunel-svid-sts:8080/
```

```json
{"user_id": "@bot:example.org", "access_token": "...", "device_id": "TUWUNELSVIDSTS", "expires_in_ms": 604800000}
```

## Image

`ghcr.io/pdeslaur/tuwunel-svid-sts`, for `linux/amd64` and `linux/arm64`, tagged `latest` and with the short commit SHA. It's built with [ko](https://ko.build) on [`cgr.dev/chainguard/static`](https://images.chainguard.dev/directory/image/static/overview) by this repo's [build workflow](.github/workflows/build.yaml), and rebuilt daily to pick up base image fixes. Verify its signature with:

```sh
cosign verify ghcr.io/pdeslaur/tuwunel-svid-sts:latest \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity https://github.com/pdeslaur/tuwunel-svid-sts/.github/workflows/build.yaml@refs/heads/main
```
