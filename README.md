# ssh-mcp

Minimal SSH MCP server (stdio, one JSON-RPC message per line). Three tools:
`ssh_exec`, `ssh_sftp`, `ssh_scp`.

## Tools

`ssh_exec` — run one shell command on a remote host.

| arg | meaning |
|---|---|
| `host` | `user@host`, `user@host:port`, or a `~/.ssh/config` alias |
| `command` | passed to the remote shell |
| `stdin` | optional text piped into the command |
| `timeout_seconds` | wall clock limit in seconds. Default and max are the server ceiling (`SSH_MCP_MAX_TIMEOUT`, stock 45); a larger value is clamped, not honoured. On expiry the remote process gets SIGTERM, then SIGKILL after 2 s |
| `max_output_bytes` | combined stdout+stderr kept, default 256 KiB, max 4 MiB. Head and tail are kept, the middle is elided |
| `password` | optional; keys / `SSH_AUTH_SOCK` are preferred |

Result text looks like:

```
exit_code: 1
duration_ms: 42
--- stdout ---
...
--- stderr ---
...
```

A non-zero remote exit code is a **result, not a tool error**. `isError: true`
means the SSH transport failed (dial, auth, timeout).

`ssh_sftp` / `ssh_scp` — copy one file. Prefix the remote side with `remote:`.
Relative local paths are resolved against the server's working directory and the
absolute path is reported back. SFTP creates missing remote parent directories;
SCP quotes the remote path, so spaces are safe.

## Environment

| var | effect |
|---|---|
| `SSH_MCP_DEBUG=1` | log connect/duration/auth diagnostics to stderr |
| `SSH_MCP_KNOWN_HOSTS=/path/known_hosts` | verify host keys against this file. **Unset by default, i.e. host keys are not verified** |
| `SSH_MCP_KEY_PASSPHRASE` | passphrase for encrypted private keys |
| `SSH_MCP_MAX_TIMEOUT=45` | per-call ceiling for `ssh_exec`, in seconds. Also the default when `timeout_seconds` is omitted, and advertised verbatim in the tool description so a caller never has to guess. Clamped to 1..1800 |

Auth order: `password` argument → `SSH_AUTH_SOCK` agent → `IdentityFile` from
`~/.ssh/config` → `~/.ssh/id_ed25519`, `id_rsa`, `id_ecdsa`.

## Notes

- Requests are handled concurrently; one slow command does not block the rest.
  Output is written under a mutex, so responses may arrive out of order (match by `id`).
- Connect timeout is 15 s; the only per-request limit is `timeout_seconds` (ceiling above).
- Keep `SSH_MCP_MAX_TIMEOUT` below the MCP proxy's own exec timeout (lazy-mcp defaults to 60 s). If the proxy gives up first the caller gets an opaque `context deadline exceeded` and the remote command keeps running - ssh-mcp can only kill what it timed out itself.

## Install

```bash
go build -o /usr/local/bin/ssh-mcp .
```

## Test

```bash
go test ./...
```
