# Matrix Onboarding Bot

This service is a Matrix DM onboarding bot written in Go. It accepts a user's first DM invite, starts a one-time onboarding flow in natural language, and keeps handling later natural-language requests by forwarding each DM turn to an external A2A HTTP agent.

## Behavior

- logs in to a Matrix homeserver with either password auth or an access token
- runs a replay-safe `/sync` loop and keeps deterministic Matrix transaction IDs for bot replies
- accepts DM invites and opens onboarding automatically the first time a user brings the bot into a direct chat
- persists only bot session state locally; per-user onboarding state and shared context live in Matrix account data so deployment does not require a separate database
- keeps per-room A2A task sessions in memory and cancels them after inactivity
- keeps talking to the user in normal text messages rather than slash-style commands
- uses standard A2A messages, tasks, task states, and context IDs as its agent contract

## A2A Integration

The bot expects an A2A agent URL and resolves the agent card from `/.well-known/agent-card.json`.

For each DM turn it sends:

- the user text
- the current task ID when the room already has an active session
- the shared context ID for that user when available
- Matrix metadata such as the room ID and user ID

The response is expected to contain:

- a natural-language text reply
- standard A2A task state and task/context identifiers when the conversation remains active
- optional artifacts, with the bot falling back to the latest text artifact when the status message itself has no text

`internal/agent/mock.go` provides the mock A2A system used by the live tests.

## Configuration

Required:

- `MATRIX_HOMESERVER_URL`
- `A2A_AGENT_URL`
- one auth pair:
  - `MATRIX_USER_ID` and `MATRIX_ACCESS_TOKEN`
  - `MATRIX_USERNAME` and `MATRIX_PASSWORD`

Optional:

- `MATRIX_AUTO_JOIN_INVITES` defaults to `true`
- `MATRIX_STATE_PATH` defaults to `data/state.json`
- `BOT_SESSION_IDLE_TIMEOUT` defaults to `10m`

`MATRIX_USERNAME` can be either a localpart such as `bot` or a full Matrix user ID such as `@bot:example.com`.

## Persistence

- Local state file:
  - Matrix access token
  - device ID
  - sync cursor
  - handled-event journal
- Matrix account data:
  - per-user onboarding completion marker
  - shared A2A context ID used to link later sessions

That split keeps deployment simple while making the one-time onboarding record survive bot restarts without introducing a separate database.

## Run

```bash
go run ./cmd/matrix-bot
```

## Tests

The live test suite spins up:

- a dockerized `tuwunel` homeserver
- a live Matrix user and bot account
- the in-process mock A2A server

Run everything with:

```bash
go test ./...
```
