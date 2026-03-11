# Matrix Bot Scaffold

This directory now contains a standalone Go Matrix bot scaffold that depends on the upstream `mautrix` module instead of the local clone in `go/`.

The dependency is pinned in `go.mod` to the current upstream release:

- `maunium.net/go/mautrix v0.26.3`

There is no `replace` directive, so the scaffold will resolve `mautrix` from upstream rather than from a filesystem path.

## What the scaffold does

- connects to a Matrix homeserver with either an access token or password login
- persists session state, sync position, and a handled-event journal to a local state file
- runs a replay-safe `/sync` loop that checkpoints progress after processing rather than before
- auto-joins invite rooms by default
- responds to `!help`, `!ping`, and `!echo <text>`
- resumes after restart and responds to messages received while it was offline, as long as the homeserver still accepts the saved sync token
- uses deterministic Matrix transaction IDs for replies so replay after a crash does not double-send bot responses

## Configuration

Set `MATRIX_HOMESERVER_URL` and one of these auth pairs:

- `MATRIX_USER_ID` and `MATRIX_ACCESS_TOKEN`
- `MATRIX_USERNAME` and `MATRIX_PASSWORD`

`MATRIX_USERNAME` can be either the Matrix username localpart such as `bot`, or a full Matrix user ID such as `@bot:example.com`.

Optional settings:

- `MATRIX_COMMAND_PREFIX` defaults to `!`
- `MATRIX_AUTO_JOIN_INVITES` defaults to `true`
- `MATRIX_STATE_PATH` defaults to `data/state.json`

## Recovery behavior

- On the very first boot with no saved sync token, the bot ignores existing room history so it does not answer old messages from before it started. It still processes outstanding invites.
- After it has saved a sync token, it resumes from that point across restarts and network disconnects.
- If the access token expires and password auth is configured, the bot will log in again and continue with the saved sync cursor.

## Run

```bash
go run ./cmd/matrix-bot
```

## Notes

- The local `go/` checkout is still useful for reading source and examples, but the bot scaffold does not import it via a path override.
- The state file contains the bot's access token and should be kept private.
- If you add handlers that cause side effects outside Matrix, make those handlers idempotent as well. The scaffold currently makes Matrix replies idempotent; it cannot make arbitrary downstream side effects safe on your behalf.
