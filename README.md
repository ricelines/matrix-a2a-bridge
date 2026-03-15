# Matrix A2A Bridge

This repo contains a Matrix DM runtime that bridges a Matrix account to an upstream A2A agent.
It is not an onboarding workflow by itself. Product-specific onboarding belongs in the upstream
agent or in a separate Amber assembly that composes this bridge with the right agent.

## Behavior

- logs in to a Matrix homeserver with password auth, then persists the resulting runtime session locally for restart and recovery
- joins direct-message invites automatically
- forwards Matrix text messages to an upstream A2A HTTP endpoint
- sends the upstream agent reply back into the Matrix room as a normal text message
- keeps per-room A2A task sessions in memory and cancels them after inactivity
- persists a shared per-user A2A context ID in Matrix account data so later rooms and restarted runtimes can reuse the same context
- persists only Matrix runtime state locally; it does not require a separate database

## A2A Integration

The runtime expects an upstream A2A URL and resolves the agent card from
`/.well-known/agent-card.json`.

For each Matrix DM turn it sends:

- the user text
- the current task ID when the room already has an active session
- the shared context ID for that Matrix user when available
- Matrix metadata such as the room ID and user ID

The upstream response is expected to contain:

- a natural-language text reply
- standard A2A task state and task/context identifiers when the conversation remains active
- optional artifacts, with the bridge falling back to the latest text artifact when the status message itself has no text

`internal/a2a/mock.go` provides the mock upstream A2A server used by the tests and the local Amber demo.

## CI and image publishing

GitHub Actions includes:

- `ci`: runs `gofmt`, `go vet`, and `go test ./...` on `main` and on pull requests
- `docker`: builds the Docker image on `main` and on pull requests, and pushes to `ghcr.io/<repo-owner>/matrix-a2a-bridge` on `main`

Both workflows use dependency and BuildKit caches, and cancel superseded runs on the same ref.

Image versioning is driven by `version-series.txt`.

Examples:

- `0.1.x` publishes `v0.1.0`, `v0.1.1`, ...
- `1.2.x` publishes `v1.2.0`, `v1.2.1`, ... and updates `v1.2` plus `v1`
- `1.0.0-alpha.x` publishes `v1.0.0-alpha.0`, `v1.0.0-alpha.1`, ... and updates `v1.0.0.alpha`

On each `main` push, CI checks GHCR for the next unused `x` value in the configured series, then publishes:

- `latest`
- the fully resolved version tag
- the appropriate floating semver tag or tags for that series

Pull requests build the image but do not publish tags.

## Configuration

Required:

- `MATRIX_HOMESERVER_URL`
- `UPSTREAM_A2A_URL`
- `MATRIX_USERNAME`
- `MATRIX_PASSWORD`

Optional:

- `MATRIX_STATE_PATH` defaults to `data/state.json`
- `BOT_SESSION_IDLE_TIMEOUT` defaults to `10m`

`MATRIX_USERNAME` can be either a localpart such as `bot` or a full Matrix user ID such as `@bot:example.com`.

On first start the runtime logs in with the configured password, then persists the returned Matrix
access token and device ID in its local state. Subsequent restarts reuse that stored session and
fall back to password login only if the stored token is missing or no longer valid.

## Persistence

Local state file:

- Matrix access token
- device ID
- sync cursor
- handled-event journal

Matrix account data:

- shared per-user A2A context ID

That split keeps deployment simple while still allowing the upstream agent context to survive bot
restarts and new DM rooms.

## Run

```bash
go run ./cmd/matrix-bot
```

## Amber Manifests

- `amber/matrix-a2a-bridge.json5` is the generic component manifest for this runtime
- `amber/mock-demo.json5` composes the bridge with the bundled mock A2A server for local testing
- `amber/codex-a2a-demo.json5` is an example composition with `codex-a2a`

If you want an onboarding experience, compose this bridge with an onboarding-oriented upstream
agent in a different repo. That assembly does not belong here.

### Example: Codex A2A Demo

Start a Matrix homeserver separately. The bridge does not create its own Matrix account, so the bot
username and password must already exist before the Amber stack starts.

This example provisions both a human test user and the bot account with `tuwunel`:

```bash
mkdir -p .tmp-tuwunel-data
docker run --rm -d \
  --name matrix-a2a-bridge-tuwunel \
  -p 127.0.0.1:6167:6167 \
  -v "$PWD/.tmp-tuwunel-data:/var/lib/tuwunel" \
  -e TUWUNEL_SERVER_NAME=tuwunel.test \
  -e TUWUNEL_DATABASE_PATH=/var/lib/tuwunel \
  -e TUWUNEL_PORT=6167 \
  -e TUWUNEL_ADDRESS=0.0.0.0 \
  -e TUWUNEL_ALLOW_FEDERATION=false \
  ghcr.io/matrix-construct/tuwunel:v1.5.1 \
  --execute "users create_user alice alice-password" \
  --execute "users create_user bot bot-password"
```

Compile the Amber example:

```bash
amber check amber/codex-a2a-demo.json5
amber compile amber/codex-a2a-demo.json5 \
  --docker-compose /tmp/matrix-a2a-bridge-compose
```

Fill in `/tmp/matrix-a2a-bridge-compose/env.example` and then start the generated stack:

```dotenv
AMBER_CONFIG_MATRIX_USERNAME=bot
AMBER_CONFIG_MATRIX_PASSWORD=bot-password
AMBER_CONFIG_BOT_SESSION_IDLE_TIMEOUT=10m
AMBER_EXTERNAL_SLOT_MATRIX_URL=127.0.0.1:6167
```

`AMBER_CONFIG_AUTH_JSON` should contain the contents of `~/.codex/auth.json`.

```bash
cd /tmp/matrix-a2a-bridge-compose
cp env.example .env
$EDITOR .env
docker compose up -d
amber proxy /tmp/matrix-a2a-bridge-compose --slot matrix=127.0.0.1:6167
```

Once the stack is up, sign into Element against `http://127.0.0.1:6167`, start a DM with
`@bot:tuwunel.test`, and send a message. The bridge should join the room and forward the chat to
the upstream A2A agent.

## Tests

Unit and in-process tests:

```bash
GOCACHE=$(pwd)/.cache/go-build go test ./...
```

Live Matrix tests:

```bash
MATRIX_A2A_BRIDGE_RUN_LIVE=1 GOCACHE=$(pwd)/.cache/go-build go test ./internal/bot
```
