# WhatsApp MCP Server

A single Go binary that serves a multi-tenant [Model Context Protocol](https://modelcontextprotocol.io/) server for WhatsApp.

Each person who wants to use it adds the server to their MCP client and signs in with GitHub when the client asks — there are no passwords or tokens to copy around. They then link their own WhatsApp number from inside the MCP client by calling the `link_whatsapp` tool — no separate bridge process, no per-user config files. All chat history, accounts and WhatsApp sessions are stored in **PostgreSQL** or **SQLite**.

It connects to WhatsApp directly via the WhatsApp Web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library), so no WhatsApp Business API account is needed. The MCP protocol itself (streamable-HTTP transport, sessions, tool schemas) is handled entirely by the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), including its `auth` package for bearer-token verification and OAuth protected resource metadata.

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that prompt injection could lead to private data exfiltration.

## Architecture

- **One binary, one process.** The old two-process design (a Go "bridge" talking to a Python MCP server over a REST API, each with its own SQLite database) is gone. Everything — HTTP API, MCP server, and the WhatsApp client — runs in one Go program.
- **Multi-tenant.** Every GitHub user on the allowlist gets their own account on the same server. Each user's chat history and linked WhatsApp device are isolated from every other user's.
- **Auth: OAuth, signed in with GitHub.** The server follows the [MCP authorization spec](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization): it is its own small OAuth 2.1 authorization server, and MCP clients discover it (protected resource and authorization server metadata), register themselves (dynamic client registration) and sign the user in (authorization code flow with PKCE) on their own. The sign-in itself is delegated to GitHub, and only the GitHub logins in `GITHUB_ALLOWED_USERS` get through. After GitHub, the server shows a consent page naming the MCP client, so a client registered by someone else can't silently get a token for your account. The resulting short-lived access token is verified on every MCP request by the MCP SDK's `auth.RequireBearerToken` middleware, which makes the user id available to every tool handler.
- **Linking a WhatsApp number happens over MCP, not a side channel.** After signing in, a user calls the `link_whatsapp` tool, which returns a QR code (as an image, to scan with WhatsApp > Linked Devices) or, if they pass their phone number, an 8-character pairing code (to enter under Linked Devices > Link with phone number). Call it again if the code expires before you've used it.
- **Storage.** PostgreSQL or SQLite holds user accounts, the mapping from user to linked WhatsApp JID, WhatsApp's own session/key material (via whatsmeow's `sqlstore`), and chat/message history.

## Running it

### Prerequisites

- Go 1.26+ (only if building from source)
- PostgreSQL 14+ (*optional*) — SQLite is used when `DATABASE_URL` is unset or points at a file
- FFmpeg (*optional*) — only needed for `send_audio_message` to convert arbitrary audio into the Opus/Ogg format WhatsApp voice messages require. Without it, `send_file` still works for raw audio files.

### Create a GitHub OAuth app

Users sign in with GitHub, so the server needs a GitHub OAuth app:

1. Go to GitHub > Settings > Developer settings > OAuth Apps > **New OAuth App** (or https://github.com/settings/applications/new).
2. Set **Homepage URL** to your server's public URL, for example `https://your-domain.ngrok-free.app`.
3. Set **Authorization callback URL** to that URL followed by `/oauth/github/callback`. The server also prints the exact callback URL it expects when it starts.
4. Register the app, then generate a client secret. Keep the **Client ID** and **Client secret** for the next step.

The callback URL must match the server's public URL, so the server needs a stable public URL. With ngrok, set `NGROK_DOMAIN` (every ngrok account includes one free static domain).

### With Docker Compose

```bash
cp .env.example .env   # set JWT_SECRET, the GITHUB_* variables and PUBLIC_URL
docker compose up -d
```

This starts Postgres and the server together; the server listens on `:8080`.

### From source

```bash
export JWT_SECRET="a long random string"
export GITHUB_CLIENT_ID="<your GitHub OAuth app's client ID>"
export GITHUB_CLIENT_SECRET="<your GitHub OAuth app's client secret>"
export GITHUB_ALLOWED_USERS="your-github-login,a-friend"   # who may sign in
# the URL clients reach the server at; defaults to http://localhost:<port>,
# and is ignored with ngrok, which uses the tunnel's URL
export PUBLIC_URL="https://whatsapp.example.com"
# optional: defaults to a SQLite file at store/whatsapp-mcp.db
export DATABASE_URL="postgres://user:pass@localhost:5432/whatsapp_mcp?sslmode=disable"
# optional, shown with their defaults:
export HTTP_ADDR=":8080"
export MEDIA_DIR="store/media"

go run ./cmd/whatsapp-mcp
```

On startup the server creates its own tables (and whatsmeow's) in the target database automatically — no separate migration step is needed.

### Choosing a database

`DATABASE_URL` picks the database engine:

| `DATABASE_URL` | Engine |
| --- | --- |
| unset | SQLite at `store/whatsapp-mcp.db` |
| `sqlite:path/to/file.db` (or `file:…`, or a bare path ending in `.db`/`.sqlite`/`.sqlite3`) | SQLite |
| `postgres://…` or `postgresql://…` | PostgreSQL |

SQLite is the easiest choice for one machine and a few users: you need no database server, and the driver is pure Go, so `CGO_ENABLED=0` builds still work. The server turns on foreign keys, WAL mode and a busy timeout automatically. To override one of these, add your own `_pragma=…` option to the URL. Use PostgreSQL when you run many users or more than one server. The two engines do not share data, and the server does not migrate data between them.

### Exposing it publicly with ngrok

Both the MCP endpoint and WhatsApp's own pairing flow need to be reachable from wherever your MCP client and phone are, which usually means a public address. If you're self-hosting (no public IP, behind NAT/a firewall, don't want to set up a reverse proxy or TLS certs), set `NGROK_AUTHTOKEN` and the server listens on a public [ngrok](https://ngrok.com) tunnel instead of a local port, using the [ngrok Go SDK](https://github.com/ngrok/ngrok-go) — no separate `ngrok` process or port-forwarding needed:

```bash
export NGROK_AUTHTOKEN="<your token from https://dashboard.ngrok.com/get-started/your-authtoken>"
# a reserved domain, so the URL (and the GitHub callback URL) is stable
# across restarts
export NGROK_DOMAIN="your-reserved-domain.ngrok-free.app"

go run ./cmd/whatsapp-mcp
# whatsapp-mcp listening on public ngrok tunnel: https://your-reserved-domain.ngrok-free.app
```

`HTTP_ADDR` and `PUBLIC_URL` are ignored while `NGROK_AUTHTOKEN` is set. With Docker Compose, set the same variables in `.env` instead (see `.env.example`); the `ports:` mapping becomes unnecessary (it's harmless to leave it either way).

#### Getting secrets from dotvault instead of env vars

If you keep secrets in [dotvault](https://github.com/goodtune/dotvault) rather than plain environment variables, pass `--ngrok-dotvault <field>` instead of setting `NGROK_AUTHTOKEN`:

```bash
dotvault login   # once, interactively — see dotvault's own docs
go run ./cmd/whatsapp-mcp --ngrok-dotvault authtoken
```

This reads the `authtoken` field of your dotvault `ngrok` secret (`kv/users/<you>/ngrok`, field `authtoken`) using a cached login — it never opens a browser or prompts, so it's safe to run unattended (e.g. under systemd), but it does mean `dotvault login` must already have a usable cached token (or peer-socket/certificate auth configured) before this process starts. It overrides `NGROK_AUTHTOKEN` if both are set. `NGROK_DOMAIN` is unaffected and still comes from the environment.

In the same way, `--github-dotvault <secret>` reads the GitHub OAuth app's credentials from the `client_id` and `client_secret` fields of that dotvault secret, instead of `GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET`:

```bash
go run ./cmd/whatsapp-mcp --ngrok-dotvault authtoken --github-dotvault github-oauth
```

Pulling in dotvault's client adds a meaningful amount to this binary (its auth layer links in TPM/confidential-computing attestation libraries several layers down, regardless of whether you ever use those auth methods) — expect roughly +15-20MB over a build without dotvault. If that's unwelcome and you don't need dotvault, just use the environment variables directly.

## Connecting a client

### 1. Add the server to your MCP client

Add the server to your MCP client as a remote (streamable-HTTP) MCP server, with the URL `<public URL>/mcp`, for example `https://your-domain.ngrok-free.app/mcp`. Don't configure any headers or tokens.

### 2. Sign in

The first time the client connects, it gets a `401` that points it at the server's OAuth metadata, and it starts the sign-in on its own:

1. A browser window opens and sends you to GitHub. Sign in, and authorize the app the first time.
2. Back on the server, check the consent page. It names the MCP client and where it will send you back to. Click **Allow**.
3. The browser returns to your MCP client, which is now connected.

The client renews its access token with a refresh token, so you only sign in again if you stop using it for 30 days. Only GitHub users listed in `GITHUB_ALLOWED_USERS` can sign in.

### 3. Link your WhatsApp number

Once connected, ask your assistant to link WhatsApp (or call the tool yourself): `link_whatsapp`. Keep calling it every few seconds until it reports you're linked:

- With no arguments, it returns a QR code image — scan it with your phone under WhatsApp > Linked Devices > Link a Device.
- With `phone_number` set to your number (digits only, country code, no `+`), it instead returns a short pairing code to type in under Linked Devices > Link with phone number.

The code/QR refreshes periodically, so if it expires before you use it, just call `link_whatsapp` again.

## MCP tools

**Account / linking**
- `link_whatsapp` — start or poll device linking (QR code or phone pairing code).
- `whatsapp_status` — whether a device is linked and currently connected.
- `whatsapp_logout` — unlink the current device.

**Contacts & chats**
- `search_contacts`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`

**Messages**
- `list_messages`, `get_message_context`

**Sending & media**
- `send_message`, `send_file`, `send_audio_message`, `download_media`

These mirror the tools from the original Python MCP server; see each tool's description (returned via `tools/list`) for its parameters.

> `send_file`, `send_audio_message` and `download_media` operate on paths on the **server's** filesystem (and `MEDIA_DIR` for downloads) — if your MCP client runs on a different machine from the server, make sure the paths you pass are reachable there.

## Development

```bash
go build ./...
go vet ./...
go test ./...   # runs against a temporary SQLite database
TEST_DATABASE_URL="postgres://…/empty_db" go test ./...   # same tests against Postgres
```

The project is a single Go module:

- `cmd/whatsapp-mcp` — entrypoint.
- `internal/config` — environment-based configuration.
- `internal/db` — Postgres/SQLite connection and per-engine schema migrations for the app's own tables.
- `internal/auth` — accounts (one per GitHub user) and access token issuing/verification.
- `internal/oauth` — the OAuth 2.1 authorization server MCP clients sign in through (metadata, dynamic client registration, GitHub sign-in, consent, tokens).
- `internal/httpapi` — the `/healthz` endpoint.
- `internal/wa` — per-user WhatsApp client lifecycle (whatsmeow), linking, sending, downloading.
- `internal/store` — database-backed chat/message repository, scoped by user.
- `internal/audioconv` — ffmpeg-based audio conversion and Ogg/Opus analysis for voice messages.
- `internal/mcpserver` — the MCP tool definitions.
