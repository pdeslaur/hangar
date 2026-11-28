# ha-config-sync

Keeps [Home Assistant](https://www.home-assistant.io)'s automations, scripts, scenes and packages in a GitHub repository, while its UI keeps editing its own. It runs beside Home Assistant, in its pod, sharing its configuration directory: it deploys the repository's commits whose [Chainguard Checks](https://www.chainguard.dev) passed, and carries the UI's edits back as pull requests, which it merges once their Checks pass.

It authenticates to GitHub with the pod's [SPIFFE](https://spiffe.io) identity, through [octo-sts](https://github.com/octo-sts/app), and to Home Assistant with nothing: `config_sync`, a small integration it installs, answers it on a Unix socket in the configuration directory.

## Who owns what

Each file has one writer, so nothing merges and nothing loops.

| Paths | Owner |
|---|---|
| `automations/`, `scripts/`, `scenes/`, `packages/` | The repository. Home Assistant shows them read-only. |
| `automations.yaml`, `scripts.yaml`, `scenes.yaml` | The UI, whose editors write only these. The repository receives them. |
| `configuration.yaml`, `.storage/` and the rest | Neither: never deployed, never synced. |

`configuration.yaml` loads both, with labeled keys, and enables the integration:

```yaml
homeassistant:
  packages: !include_dir_named packages/

automation ui: !include automations.yaml
automation code: !include_dir_merge_list automations/
script ui: !include scripts.yaml
script code: !include_dir_merge_named scripts/
scene ui: !include scenes.yaml
scene code: !include_dir_merge_list scenes/

config_sync:
```

## How it works

- **The integration.** As it starts, before Home Assistant, it writes `config_sync` into `custom_components/` and tells systemd it's ready. Home Assistant loads an integration's code only as it starts, so a new version takes a restart. The integration serves HTTP on `.config_sync/api.sock`: `POST /check` runs Home Assistant's configuration check, `POST /reload` the reload services of `automation`, `script`, `scene`, or `homeassistant.reload_all`, `POST /failed` and `POST /succeeded` raise or dismiss a persistent notification and fire `config_sync_failed` or `config_sync_succeeded`, with `kind` (`deploy` or `sync`), `sha` and `message`, for automations to forward.
- **Deploying.** Every `POLL_INTERVAL`, it asks for `BRANCH`'s head with its last ETag, which costs no rate limit while the branch hasn't moved. A new commit waits for its `Chainguard Checks` check runs: when they fail, or none pass within `CHECKS_TIMEOUT`, it skips the commit. Once they pass, it extracts the commit's owned directories, regular files only, swaps those that changed into place with renames, and has Home Assistant check the configuration. A refused configuration is rolled back, reported, and the commit skipped; an accepted one is reloaded, `homeassistant.reload_all` when `packages/` changed, since a package can hold any integration. It remembers the commits it applied and skipped in `.config_sync/state.json`.
- **Syncing.** It watches the configuration directory for the UI's files, and `DEBOUNCE` after the last write, or as it starts, compares them with `BRANCH`'s by their git hashes. When they differ, it commits them on `BRANCH` through the Git Data API, moves `SYNC_BRANCH` to that commit by force, and opens a pull request if none is open. Once the commit's Checks pass, it squash-merges it, naming the commit it checked, so a newer one is never merged unchecked. When they fail, it leaves the pull request open, to show why, and reports it. When the UI's files match `BRANCH` again, it closes the pull request.
- **Without branch protection.** It works on private repositories of GitHub's free plan, which have no rulesets or auto-merge: it waits for the Checks itself, both before merging and before deploying, whoever pushed.

## Trust

- **GitHub.** It fetches a JWT-SVID for `OCTO_STS_AUDIENCE` from the Workload API and exchanges it at octo-sts for a token of the repository's trust policy, `.github/chainguard/<IDENTITY>.sts.yaml`, which it reuses for 45 minutes. The policy needs `contents: write` and `pull_requests: write` to sync, and `checks: read`:

  ```yaml
  issuer: https://oidc.example.org
  subject: spiffe://example.org/svc/homeassistant
  permissions:
      contents: write
      pull_requests: write
      checks: read
  ```

  octo-sts must reach the issuer's discovery document. Whoever holds the pod's identity can push to the repository, as the pod already controls Home Assistant.
- **Home Assistant.** The socket, `0600` in a `0700` directory, is reachable by the containers sharing the configuration directory as the same user, Home Assistant's and this one. It takes no token: a long-lived access token would be an administrator's credential that never expires.

## Configuration

| Variable | Default | |
|---|---|---|
| `REPOSITORY` | | The configuration's repository, `owner/name`. Required. |
| `CONFIG_DIR` | `/config` | Home Assistant's configuration directory. |
| `BRANCH` | `main` | The branch it deploys, and merges the UI's edits into. |
| `SYNC_BRANCH` | `ha-ui-sync` | The branch carrying the UI's edits, moved by force. |
| `IDENTITY` | `ha-config-sync` | The repository's octo-sts trust policy. |
| `OCTO_STS_URL` | `https://octo-sts.dev` | octo-sts. |
| `OCTO_STS_AUDIENCE` | `octo-sts.dev` | The JWT-SVIDs' audience. |
| `GITHUB_API` | `https://api.github.com` | GitHub's REST API. |
| `POLL_INTERVAL` | `30s` | How often it looks for new commits and Checks. |
| `DEBOUNCE` | `30s` | How long the UI's files stay unwritten before it syncs. |
| `CHECKS_TIMEOUT` | `30m` | How long it waits for a commit's Checks. |
| `SPIFFE_ENDPOINT_SOCKET` | | The Workload API, like `unix:///run/spire/agent/api.sock`. |

Run it as Home Assistant's user, so that the files it deploys are Home Assistant's.

## Testing

`internal/haconfig`'s tests run against a fake GitHub and a fake integration. The integration's own test runs it in a real Home Assistant, with docker, when `HOMEASSISTANT_IMAGE` names one:

```sh
HOMEASSISTANT_IMAGE=ghcr.io/home-assistant/home-assistant:stable go test ./internal/haconfig/...
```

## Image

`ghcr.io/pdeslaur/ha-config-sync`, for `linux/amd64` and `linux/arm64`, tagged `latest` and with the short commit SHA. See [Images](../../README.md#images) for how it's built and how to verify its signature.
