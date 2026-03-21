# Matrix A2A Bridge

This repo contains a Matrix event ingress runtime. It logs in as one Matrix account, syncs that
account's room traffic, and delivers relevant Matrix events to an upstream A2A agent.

It is not a chat bot runtime. It does not auto-join rooms, it does not synthesize replies, and it
does not own conversational state on behalf of the upstream agent.

## Boundary

`matrix-a2a-bridge` is the Matrix-to-agent side of the integration.

- It forwards Matrix events upstream.
- It persists Matrix login and sync state locally so it can resume cleanly.
- It ignores events authored by its own Matrix account so agent-driven Matrix writes do not loop.

The peer for agent-driven Matrix actions is `../matrix-mcp`.

- `matrix-a2a-bridge` tells the agent that something happened on Matrix.
- `matrix-mcp` gives the agent the tool surface it needs to inspect rooms and write back to Matrix.

That split keeps this component generic. The agent decides whether to join a room, what context to
fetch, and what to send back.

## Behavior

- logs in to a Matrix homeserver with password auth, then persists the resulting runtime session locally for restart and recovery
- syncs Matrix room state and timeline events for the configured account
- forwards joined-room timeline events, joined-room state events, invite-state events, and left-room state or timeline events to the upstream A2A endpoint
- skips initial joined-room backlog on a fresh state file so startup does not replay old traffic
- preserves outstanding invites on first sync so the upstream agent can decide whether to join
- ignores events sent by the bridge account itself to avoid feedback loops with `matrix-mcp`
- persists only Matrix runtime state locally; it does not require a separate database

## Event Delivery

Each forwarded event is sent upstream as a non-blocking A2A notification.

The request body is a JSON envelope with:

- `kind: "matrix_event"`
- `bridge_user_id`
- `homeserver_url`
- `source.room_section`: `join`, `invite`, or `leave`
- `source.event_section`: `timeline` or `state`
- `event`: the Matrix event payload

The same envelope is also attached under `metadata.matrix_event`.

The bridge does not wait for a conversational reply and does not post anything back into Matrix.

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

No upstream A2A task IDs, room sessions, or per-user context are stored here. Those concerns belong
in the upstream agent.

## Run

```bash
go run ./cmd/matrix-a2a-bridge
```

## Amber Manifests

- `amber/matrix-a2a-bridge.json5` is the generic component manifest for this runtime
- `amber/mock-demo.json5` composes the bridge with the bundled mock A2A server for local ingress testing
- `amber/codex-a2a-demo.json5` is an ingress-only example composition with `codex-a2a`

If you want the upstream agent to respond on Matrix, compose the bridge with `matrix-mcp` as a peer
tool surface for that same Matrix account.

### Example: Codex A2A Ingress Demo

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

Once the stack is up, sign into Element against `http://127.0.0.1:6167`, create or invite
`@bot:tuwunel.test` into a room, and send traffic there. The bridge will forward those Matrix
events to the upstream A2A agent. For a full read/write agent workflow, pair that agent with
`matrix-mcp`.

## Tests

Unit and in-process tests:

```bash
GOCACHE=$(pwd)/.cache/go-build go test ./...
```

Live Matrix tests:

```bash
MATRIX_A2A_BRIDGE_RUN_LIVE=1 GOCACHE=$(pwd)/.cache/go-build go test ./internal/bridge
```
