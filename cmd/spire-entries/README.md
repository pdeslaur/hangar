# oci-spire-entries

Makes a [SPIRE](https://spiffe.io/docs/latest/spire-about/) server's registration entries match a JSON file, then exits. It's meant to run beside the server each time the server starts, so that the file, generated from configuration kept in git, is the source of truth for which workloads get which identities, outside Kubernetes, where [spire-controller-manager](https://github.com/spiffe/spire-controller-manager) doesn't apply.

It talks to the server's admin API over its unix socket and:

- creates the entries the file declares that the server lacks;
- updates those whose SPIFFE ID, parent ID or selectors differ;
- deletes the entries it owns that the file no longer declares.

It owns the entries whose IDs start with `ID_PREFIX`, and every entry in the file must have one. Entries without the prefix, such as ones created by hand, are left alone.

## Entries

A JSON array:

```json
[
    {
        "id": "managed-web",
        "spiffeID": "spiffe://example.org/web",
        "parentID": "spiffe://example.org/spire/agent/x509pop/host1",
        "selectors": ["systemd:id:user@1001.service"]
    }
]
```

Selectors are `type:value`, split at the first colon, so the above is the `systemd` selector `id:user@1001.service`. Other entry fields keep the server's defaults.

## Configuration

| Variable | Default | |
|---|---|---|
| `ENTRIES_FILE` | | The JSON file. Required. |
| `ID_PREFIX` | | The prefix of the entry IDs it owns. Required. |
| `SOCKET_PATH` | `/tmp/spire-server/private/api.sock` | The server's admin socket. |
| `TIMEOUT` | `2m` | Bounds the run, including waiting for the server to listen. |

It exits non-zero if any entry fails, after attempting all of them.

## Image

`ghcr.io/pdeslaur/oci-spire-entries`, for `linux/amd64` and `linux/arm64`, tagged `latest` and with the short commit SHA. It's built with [ko](https://ko.build) on [`cgr.dev/chainguard/static`](https://images.chainguard.dev/directory/image/static/overview) by this repo's [build workflow](.github/workflows/build.yaml), and rebuilt daily to pick up base image fixes. Verify its signature with:

```sh
cosign verify ghcr.io/pdeslaur/oci-spire-entries:latest \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity https://github.com/pdeslaur/oci-spire-entries/.github/workflows/build.yaml@refs/heads/main
```

## Usage

The container needs the server's admin socket, and the same uid as the server, to connect to it:

```sh
podman run --rm \
    -e ENTRIES_FILE=/etc/entries.json \
    -e ID_PREFIX=managed- \
    -e SOCKET_PATH=/run/spire/admin/api.sock \
    -v ./entries.json:/etc/entries.json:ro \
    -v spire-admin:/run/spire/admin \
    ghcr.io/pdeslaur/oci-spire-entries:latest
```
