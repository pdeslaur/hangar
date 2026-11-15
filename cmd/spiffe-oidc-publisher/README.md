# spiffe-oidc-publisher

Publishes a [SPIFFE](https://spiffe.io) trust domain's JWT keys as an OIDC issuer's static documents, in an S3-compatible bucket served on the issuer's URL, like Cloudflare R2 on a custom domain. Relying parties that verify JWT-SVIDs through OIDC discovery, like GCP's and AWS's workload identity federation, read them there: nothing of the trust domain is reachable from the internet. It's the static counterpart of SPIRE's `oidc-discovery-provider`, which serves the same documents from a running process.

## How it works

- **Keys.** It watches the trust domain's JWT bundle through the Workload API, which sends a new one whenever the keys change, such as when the server prepares its next key.
- **Documents.** For each bundle it writes two objects, `application/json`:
  - `jwks`: the bundle's JWT keys, each with `use: "sig"` and the `alg` its type implies (`ES256`, `ES384`, `ES512`, `RS256` or `EdDSA`), sorted by key ID. SPIFFE's own bundle format marks them `use: "jwt-svid"` and names no algorithm, which verifiers like GCP's STS reject (`Unable to verify the ID Token signature`).
  - `.well-known/openid-configuration`: the issuer, `jwks_uri: <issuer>/jwks`, the keys' algorithms, and the `id_token` response and `public` subject types.
- **Order.** The JWKS goes first, so the discovery document never names keys that aren't there yet. Only documents that changed are written; every `REFRESH` both are written again, so an object deleted or overwritten in the bucket comes back.
- **Failures.** A failed write is logged, and tried again with the next bundle or at the next refresh. The documents in the bucket stay as they were, and relying parties keep verifying with them until the server signs with a key they don't hold: SPIRE prepares each key long before signing with it.

## Trust

Whoever can write the bucket can mint tokens its relying parties accept, so its credentials belong with the trust domain's signing keys: run it beside the SPIRE server, sharing the credentials of its bundle publisher, if it has one. Its SPIFFE ID needs no registration entry of its own beyond one the Workload API answers: any workload of the trust domain gets the bundle.

## Configuration

| Variable | Default | |
|---|---|---|
| `TRUST_DOMAIN` | | The trust domain whose JWT keys are published, like `example.com`. Required. |
| `ISSUER` | | The issuer's URL, which the bucket is served on, like `https://oidc.example.com`. Required. |
| `ENDPOINT` | | The bucket's S3 endpoint, like `https://<account>.r2.cloudflarestorage.com`. Required. |
| `BUCKET` | | The bucket. Required. |
| `REGION` | `auto` | The bucket's region; R2's is `auto`. |
| `REFRESH` | `1h` | How often both documents are written again, changed or not. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | | Credentials writing the bucket's objects. |
| `SPIFFE_ENDPOINT_SOCKET` | | The Workload API, like `unix:///run/spire/agent/api.sock`. |

It tells systemd it's ready, through `NOTIFY_SOCKET`, once it has published the first bundle.

## Image

`ghcr.io/pdeslaur/spiffe-oidc-publisher`, for `linux/amd64` and `linux/arm64`, tagged `latest` and with the short commit SHA. See [Images](../../README.md#images) for how it's built and how to verify its signature.
