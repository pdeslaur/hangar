# keybroker

Derives each workload's secrets from one master secret and the workload's [SPIFFE](https://spiffe.io) ID, for secrets only the workload itself uses: encryption keys, peppers, session-signing keys. No secret is stored but the master, and a workload gets its own secrets and no one else's.

```
key = HKDF-SHA256(master, salt = <trust domain name>, info = <SPIFFE ID> || 0x00 || <name> || 0x00 || <version>)
```

Names hold letters, digits, `_`, `-` and `.`, in segments separated by `/`, so none contains a NUL, and two different (ID, name, version) triples never give the same `info`. Raising a secret's version changes that secret alone.

## serve

`keybroker serve` listens with mTLS, as its X509-SVID from the Workload API at `SPIFFE_ENDPOINT_SOCKET`, accepting the SVIDs of its own trust domain whose path starts with `/svc/`. For

```
GET /v1/secrets/{name}?version=N&bytes=B
```

it returns, raw, the secret for the SPIFFE ID in the client's verified certificate. `version` defaults to 1 and `bytes` to 32, between 16 and 64. The request names no ID, so no request reaches another workload's secret.

| Variable | |
|---|---|
| `ADDR` | Where it listens; `:8443` by default. |
| `MASTER_FILE` | The master: 32 bytes. It creates a random one, mode `0600`, when the file is missing, and refuses to start when it holds anything but 32 bytes. |

It tells systemd it's ready, through `NOTIFY_SOCKET`, once it's listening.

The master is the one secret worth backing up: losing it loses every derived secret, and whatever they encrypt. It never rotates: a new master changes every secret at once.

## fetch

`keybroker fetch` fetches a workload's secrets, as its X509-SVID, and writes them to env files, one per variable, mode `0600`, replacing each atomically and removing the other `.env` files in the directory. It waits for the Workload API, and retries with backoff while the broker is unreachable or failing, until it's stopped; the broker refusing a request stops it.

| Variable | |
|---|---|
| `BROKER_URL` | The broker's base URL, like `https://10.0.0.1:8443`. |
| `BROKER_ID` | The SPIFFE ID the broker must present. |
| `DIR` | Where it writes the files. |
| `FILES` | The files, as JSON. |

Each file sets a variable to its parts, concatenated: text, or a derived secret, encoded as `hex`, the default, or `base64`.

```json
[
  {"name": "ENCRYPTION_KEY.env", "variable": "ENCRYPTION_KEY",
   "parts": [{"derive": {"name": "ENCRYPTION_KEY"}}]},
  {"name": "DATABASE_URL.env", "variable": "DATABASE_URL",
   "parts": [
     {"text": "postgresql://app:"},
     {"derive": {"name": "POSTGRES_PASSWORD", "version": 1, "bytes": 32, "encoding": "hex"}},
     {"text": "@127.0.0.1:5432/app"}
   ]}
]
```
