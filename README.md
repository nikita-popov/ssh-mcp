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
| `timeout_seconds` | wall clock limit, default 60, max 1800. On expiry the channel is torn down and the remote process gets SIGHUP |
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

Auth order: `password` argument → `SSH_AUTH_SOCK` agent → `IdentityFile` from
`~/.ssh/config` → `~/.ssh/id_ed25519`, `id_rsa`, `id_ecdsa`.

## Notes

- Requests are handled concurrently; one slow command does not block the rest.
  Output is written under a mutex, so responses may arrive out of order (match by `id`).
- Connect timeout is 15 s. There is no per-request timeout other than `timeout_seconds`.

## Install

```bash
go build -o /usr/local/bin/ssh-mcp .
```

## Test

```bash
go test ./...
```
