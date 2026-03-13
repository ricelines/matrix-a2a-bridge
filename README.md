# Matrix Onboarding Bot

This service is a Matrix DM onboarding bot written in Go. It accepts a user's DM invite, starts a one-time onboarding flow when the user sends the first message in the DM, and keeps handling later natural-language requests by forwarding each DM turn to an external A2A HTTP agent.

## Behavior

- logs in to a Matrix homeserver with password auth, then persists the resulting session locally for restart and recovery
- runs a replay-safe `/sync` loop and keeps deterministic Matrix transaction IDs for bot replies
- joins DM invites automatically and opens onboarding when the user sends the first message in a direct chat
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
- `MATRIX_USERNAME`
- `MATRIX_PASSWORD`

Optional:

- `MATRIX_STATE_PATH` defaults to `data/state.json`
- `BOT_SESSION_IDLE_TIMEOUT` defaults to `10m`

`MATRIX_USERNAME` can be either a localpart such as `bot` or a full Matrix user ID such as `@bot:example.com`.
On first start the bot logs in with the configured password, then persists the returned Matrix access token and device ID in its local state. Subsequent restarts reuse that stored session and only fall back to password login if the stored token is missing or no longer valid.

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

## Amber Scenario

This repo also includes an Amber scenario that runs the onboarding bot against a `codex-a2a`
agent while leaving the Matrix homeserver outside the scenario. The checked-in scenario lives at
`amber/onboarding-codex-a2a.json5` and is parameterized at the root rather than hardcoding local
credentials.

Its root config schema accepts:

- `auth_json`
- `agents_md`
- `matrix_username`
- `matrix_password`
- `bot_session_idle_timeout`

Amber wires those root config values into the generated Docker Compose runtime as environment
variables. When you compile this scenario, Amber emits an `env.example` containing:

- `AMBER_CONFIG_AUTH_JSON`
- `AMBER_CONFIG_MATRIX_USERNAME`
- `AMBER_CONFIG_MATRIX_PASSWORD`
- `AMBER_CONFIG_BOT_SESSION_IDLE_TIMEOUT`
- `AMBER_CONFIG_AGENTS_MD`

That is the runtime config path for this scenario. `~/.codex/config.toml` is not required and
should not be forwarded for this setup.

### Register The Bot On Tuwunel

Start `tuwunel` separately from the Amber stack. The onboarding bot does not create its own Matrix
account, so the bot username and password must already exist on the homeserver before the Amber
stack starts.

This example provisions both a human test user and the bot account at tuwunel startup:

```bash
mkdir -p .tmp-tuwunel-data
docker run --rm -d \
  --name onboarding-tuwunel \
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

If you already have a local tuwunel container, create the bot account there first. The important
invariant is simple: the `matrix_username` and `matrix_password` you pass into the Amber scenario
must match an already-registered Matrix account on the external tuwunel server.

### Compile And Run The Amber Stack

Compile the checked-in scenario directly:

```bash
amber check amber/onboarding-codex-a2a.json5
amber compile amber/onboarding-codex-a2a.json5 \
  --docker-compose /tmp/onboarding-amber-compose
```

Amber will generate `/tmp/onboarding-amber-compose/env.example`. Fill in the required runtime
config there. The bot account values must match the Matrix account you provisioned on tuwunel:

```dotenv
AMBER_CONFIG_MATRIX_USERNAME=bot
AMBER_CONFIG_MATRIX_PASSWORD=bot-password
AMBER_CONFIG_BOT_SESSION_IDLE_TIMEOUT=10m
AMBER_CONFIG_AGENTS_MD="Use repository instructions."
AMBER_EXTERNAL_SLOT_MATRIX_URL=127.0.0.1:6167
```

For `AMBER_CONFIG_AUTH_JSON`, pass the contents of `~/.codex/auth.json` as the runtime config
value. Amber expects the raw file contents, not a path and not `config.toml`.

Then start the stack from the generated compose directory:

```bash
cd /tmp/onboarding-amber-compose
cp env.example .env
$EDITOR .env
docker compose up -d
```

Wait until `amber-router`, `c2-bot-net`, and `c1-agent` are up in `docker ps`, then attach the
external Matrix slot to the tuwunel server that is already running on the host:

```bash
amber proxy /tmp/onboarding-amber-compose \
  --slot matrix=127.0.0.1:6167
```

Leave `amber proxy` running. It is the bridge between the Amber router and the external Matrix
homeserver.

Run the proxy with `-v` or `-vv` if you want an explicit confirmation that the slot attached
correctly. A healthy attachment prints a line like:

```text
registered slot matrix via router control (...)
```

The bot attempts Matrix password login immediately on startup and exits if the Matrix slot is not
ready yet. If `c2-bot` logs `HTTP 502` or `HTTP 503` from `/_matrix/client/v3/login`, leave
`amber proxy` running after it prints the registration line, then restart just the bot container:

```bash
docker compose up -d c2-bot
```

To tear the scenario down later:

```bash
docker compose -f /tmp/onboarding-amber-compose/compose.yaml down
docker stop onboarding-tuwunel
```

### Test With Element

Element Desktop is the easiest option for this local setup because you can point it directly at
the plain HTTP homeserver URL.

1. Open Element Desktop.
2. Choose the custom homeserver option and set the homeserver URL to `http://127.0.0.1:6167`.
3. Sign in as `alice` with password `alice-password`.
4. Start a direct chat with `@bot:tuwunel.test`.
5. Send any message. The bot should join the DM and begin the onboarding flow with the welcome
   prompt.
6. Finish the onboarding prompts, then send a normal follow-up question to confirm that the bot is
   forwarding later turns to the upstream codex-a2a agent.

If the bot does not answer, check these first:

- `amber proxy` is still running, points at `127.0.0.1:6167`, and printed `registered slot matrix via router control`
- the bot account exists on tuwunel and the password matches the scenario manifest
- `docker ps -a` still shows `c2-bot` as running; if it exited after `HTTP 502` or `HTTP 503`, restart it with `docker compose up -d c2-bot`
- `docker compose -f /tmp/onboarding-amber-compose/compose.yaml logs c2-bot`
- `docker compose -f /tmp/onboarding-amber-compose/compose.yaml logs c1-agent`

## Tests

`go test ./...` runs the default unit and in-process coverage.

The live bot suite is opt-in and spins up:

- a dockerized `tuwunel` homeserver
- a live Matrix user and bot account
- the in-process mock A2A server

Run it with:

```bash
ONBOARDING_RUN_LIVE=1 go test ./internal/bot
```
