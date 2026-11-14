# oidc-relay

Authenticates an app's OIDC client with the pod's [SPIFFE](https://spiffe.io) identity, for an app that does OIDC itself and can only send a client secret, like Spliit. It runs beside the app, in its pod: the app reads the provider's discovery document from the relay, which points the app's token requests back at it, and the relay sends them on with a JWT-SVID in place of the secret. The provider trusts the SVID through the client's federated identity, as [PocketID](https://pocket-id.org) does, so the client needs no secret at all.

## How it works

- **Discovery.** `GET /.well-known/openid-configuration` answers the issuer's own document, fetched for each request, with `token_endpoint` replaced by `http://<the host the app asked>/token`. Everything else stays the issuer's: the issuer, its authorization and userinfo endpoints and its JWKS, so the browser still logs in at the provider and ID tokens verify as before.
- **Tokens.** `POST /token` takes a token request authenticated either way a secret is sent, `client_secret_post` or `client_secret_basic`, drops the secret, adds `client_assertion_type` (RFC 7523's JWT bearer) and a `client_assertion`, a JWT-SVID fetched from the Workload API with `ISSUER` as its audience, and sends it to the issuer's token endpoint. It answers the provider's response as it is. Without an SVID it answers `503`, and `502` without the issuer, both with RFC 6749's error body.
- **Only for apps taking a discovery URL apart from the issuer.** An app that derives the document's URL from the issuer and checks that the document names the issuer it fetched it from, as go-oidc does, can't be pointed at the relay.

## Trust

Anything in its pod authenticates as the client: it listens on loopback, without authentication or TLS. The provider still checks each SVID's subject against the client's federated identities, so the relay can only authenticate as clients that trust its pod's SPIFFE ID. It follows no redirect from the issuer, which would send assertions and codes elsewhere.

## Configuration

| Variable | Default | |
|---|---|---|
| `ISSUER` | | The provider's issuer, like `https://id.example.com`, and the audience of the JWT-SVIDs. Required. |
| `ADDR` | `127.0.0.1:8095` | Where it listens. Keep it on loopback. The app's discovery URL is `http://<ADDR>/.well-known/openid-configuration`. |
| `SPIFFE_ENDPOINT_SOCKET` | | The Workload API, like `unix:///run/spire/agent/api.sock`. |

It tells systemd it's ready, through `NOTIFY_SOCKET`, once it listens, so that an app reading the discovery document only as it starts can be ordered after it.

## Image

`ghcr.io/pdeslaur/oidc-relay`, for `linux/amd64` and `linux/arm64`, tagged `latest` and with the short commit SHA. See [Images](../../README.md#images) for how it's built and how to verify its signature.
