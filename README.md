# hangar

Go services, each built into an image with [ko](https://ko.build) and published
to `ghcr.io/pdeslaur/<service>`.

| Service | |
|---|---|
| [cloudflared-svid](cmd/cloudflared-svid) | Runs a Cloudflare tunnel with credentials cloudflare-sts hands its SPIFFE identity. |
| [matrix-relay](cmd/matrix-relay) | Keeps an end-to-end encrypted Matrix session for an app that can't. |
| [jellyfin-bridge](cmd/jellyfin-bridge) | Puts Jellyfin behind single sign-on, apps included, with tokens of its own. |
| [keybroker](cmd/keybroker) | Derives each workload's secrets from one master secret and its SPIFFE ID. |
| [oidc-relay](cmd/oidc-relay) | Authenticates an app's OIDC client with its SPIFFE identity instead of a secret. |
| [seerr-sso](cmd/seerr-sso) | Puts Seerr behind single sign-on, through jellyfin-bridge and Seerr's Jellyfin Quick Connect. |
| [smtp-relay](cmd/smtp-relay) | Sends an app's SMTP email through cloudflare-sts, with its SPIFFE identity. |
| [spiffe-oidc-publisher](cmd/spiffe-oidc-publisher) | Publishes a SPIFFE trust domain's JWT keys as static OIDC discovery documents in a bucket. |
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
or [Jellyfin](https://jellyfin.org) run with docker when `TUWUNEL_IMAGE` or
`JELLYFIN_IMAGE`, a linuxserver.io Jellyfin image, names one:

```sh
GOFLAGS=-tags=goolm \
    TUWUNEL_IMAGE=ghcr.io/matrix-construct/tuwunel:latest \
    JELLYFIN_IMAGE=lscr.io/linuxserver/jellyfin:latest \
    go test ./...
```

CI runs the tests with the race detector and against tuwunel and Jellyfin,
and checks that the code is formatted with `gofmt`, that `go.mod` is tidy,
and that `go vet`, [staticcheck](https://staticcheck.dev) and
[govulncheck](https://go.dev/doc/security/vuln/) find nothing. It builds
jellyfin-bridge's pages first, for the jobs compiling Go.

## Images

The [build workflow](.github/workflows/build.yaml) builds every command under
`cmd/` on [`cgr.dev/chainguard/static`](https://images.chainguard.dev/directory/image/static/overview),
except cloudflared-svid, which builds on
Cloudflare's [`cloudflare/cloudflared`](https://hub.docker.com/r/cloudflare/cloudflared) (see [`.ko.yaml`](.ko.yaml)),
for `linux/amd64` and `linux/arm64`, and publishes each to
`ghcr.io/pdeslaur/<command>`, tagged `latest`. It rebuilds them daily to pick up
base image fixes, and signs them with [cosign](https://github.com/sigstore/cosign)
keyless signing. The binaries don't hold the commit they're built from, so an
image's digest changes only when its code, dependencies, Go or base image do,
and a digest already signed isn't signed again. Verify one with:

```sh
cosign verify ghcr.io/pdeslaur/<command>:latest \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity https://github.com/pdeslaur/hangar/.github/workflows/build.yaml@refs/heads/main
```

[Renovate](https://github.com/pdeslaur/renovate) checks for Go module and
GitHub Actions updates daily, and its pull requests merge themselves once the
required checks pass.
