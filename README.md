# hangar

Go services, each built into an image with [ko](https://ko.build) and published
to `ghcr.io/pdeslaur/<service>`.

| Service | |
|---|---|
| [matrix-relay](cmd/matrix-relay) | Keeps an end-to-end encrypted Matrix session for an app that can't. |
| [keybroker](cmd/keybroker) | Derives each workload's secrets from one master secret and its SPIFFE ID. |
| [spire-entries](cmd/spire-entries) | Makes a SPIRE server's registration entries match a JSON file. |
| [spliitforynab](cmd/spliitforynab) | Imports Spliit Cloud group expenses into YNAB. |
| [tuwunel-svid-sts](cmd/tuwunel-svid-sts) | Exchanges SPIFFE JWT-SVIDs for Matrix access tokens. |

Each service is a command under `cmd/`, documented in its own README, with its
packages under `internal/`. They share one Go module.

## Testing

mautrix needs the `goolm` build tag for its olm in Go, so set it for the whole
module:

```sh
GOFLAGS=-tags=goolm go test ./...
```

Tests that need a real [tuwunel](https://github.com/matrix-construct/tuwunel)
run with docker when `TUWUNEL_IMAGE` names an image:

```sh
GOFLAGS=-tags=goolm TUWUNEL_IMAGE=ghcr.io/matrix-construct/tuwunel:latest go test ./...
```

CI runs the tests with the race detector and against tuwunel, and checks
that the code is formatted with `gofmt`, that `go.mod` is tidy, and that
`go vet`, [staticcheck](https://staticcheck.dev) and
[govulncheck](https://go.dev/doc/security/vuln/) find nothing.

## Images

The [build workflow](.github/workflows/build.yaml) builds every command under
`cmd/` on [`cgr.dev/chainguard/static`](https://images.chainguard.dev/directory/image/static/overview),
for `linux/amd64` and `linux/arm64`, and publishes each to
`ghcr.io/pdeslaur/<command>`, tagged `latest` and with the short commit SHA. It
rebuilds them daily to pick up base image fixes, and signs them with
[cosign](https://github.com/sigstore/cosign) keyless signing. Verify one with:

```sh
cosign verify ghcr.io/pdeslaur/<command>:latest \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity https://github.com/pdeslaur/hangar/.github/workflows/build.yaml@refs/heads/main
```

[Dependabot](.github/dependabot.yml) checks for Go module updates daily and for
GitHub Actions updates weekly.
