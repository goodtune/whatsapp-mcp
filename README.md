# WhatsApp MCP Server

A single Go binary that serves a multi-tenant [Model Context Protocol](https://modelcontextprotocol.io/) server for WhatsApp.

Each person who wants to use it registers an account on the server, logs in to get a token, then links their own WhatsApp number from inside their MCP client by calling the `link_whatsapp` tool — no separate bridge process, no per-user config files. All chat history, accounts and WhatsApp sessions are stored in **PostgreSQL**.

It connects to WhatsApp directly via the WhatsApp Web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library), so no WhatsApp Business API account is needed. The MCP protocol itself (streamable-HTTP transport, sessions, tool schemas) is handled entirely by the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), including its `auth` package for bearer-token verification — none of that is hand-rolled here.

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that prompt injection could lead to private data exfiltration.

## Architecture

- **One binary, one process.** The old two-process design (a Go "bridge" talking to a Python MCP server over a REST API, each with its own SQLite database) is gone. Everything — HTTP API, MCP server, and the WhatsApp client — runs in one Go program.
- **Multi-tenant.** Any number of people can register accounts on the same server. Each user's chat history and linked WhatsApp device are isolated from every other user's.
- **Auth.** Registration and login are plain REST endpoints that return a JWT. Every MCP tool call must carry that JWT as `Authorization: Bearer <token>`; the MCP SDK's `auth.RequireBearerToken` middleware verifies it (via a small adapter around our own JWT check) and makes the resulting user id available to every tool handler, scoping each tool call, query, and WhatsApp session to that user.
- **Linking a WhatsApp number happens over MCP, not a side channel.** After logging in, a user calls the `link_whatsapp` tool, which returns a QR code (as an image, to scan with WhatsApp > Linked Devices) or, if they pass their phone number, an 8-character pairing code (to enter under Linked Devices > Link with phone number). Call it again if the code expires before you've used it.
- **Storage.** PostgreSQL holds user accounts, the mapping from user to linked WhatsApp JID, WhatsApp's own session/key material (via whatsmeow's `sqlstore`), and chat/message history.

## Running it

### Prerequisites

- Go 1.26+ (only if building from source)
- PostgreSQL 14+
- FFmpeg (*optional*) — only needed for `send_audio_message` to convert arbitrary audio into the Opus/Ogg format WhatsApp voice messages require. Without it, `send_file` still works for raw audio files.

### With Docker Compose

```bash
cp .env.example .env   # set JWT_SECRET to a long random string
docker compose up -d
```

This starts Postgres and the server together; the server listens on `:8080`.

### From source

```bash
export DATABASE_URL="postgres://user:pass@localhost:5432/whatsapp_mcp?sslmode=disable"
export JWT_SECRET="a long random string"
# optional, shown with their defaults:
export HTTP_ADDR=":8080"
export MEDIA_DIR="store/media"

go run ./cmd/whatsapp-mcp
```

On startup the server creates its own tables (and whatsmeow's) in the target database automatically — no separate migration step is needed.

## Connecting a client

### 1. Register and log in

```bash
curl -X POST http://localhost:8080/api/register \
  -H 'Content-Type: application/json' \
  -d '{"username": "alice", "password": "a long passphrase"}'
# => {"token": "...", "username": "alice"}
```

If you already have an account, use `/api/login` with the same body instead. Either way, keep the returned `token` — it's the bearer token for the MCP endpoint and it doesn't change until you log in again.

### 2. Point your MCP client at the server

Configure your MCP client to use the streamable-HTTP transport against `http://<host>:8080/mcp`, sending `Authorization: Bearer <token>` with every request. The exact configuration surface depends on the client; for example, with a client that supports a `headers` field for HTTP MCP servers:

```json
{
  "mcpServers": {
    "whatsapp": {
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer {{TOKEN}}"
      }
    }
  }
}
```

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
```

The project is a single Go module:

- `cmd/whatsapp-mcp` — entrypoint.
- `internal/config` — environment-based configuration.
- `internal/db` — Postgres connection and schema migrations for the app's own tables.
- `internal/auth` — accounts, password hashing, JWT issuing/verification.
- `internal/httpapi` — the `/api/register`, `/api/login`, `/healthz` REST endpoints.
- `internal/wa` — per-user WhatsApp client lifecycle (whatsmeow), linking, sending, downloading.
- `internal/store` — Postgres-backed chat/message repository, scoped by user.
- `internal/audioconv` — ffmpeg-based audio conversion and Ogg/Opus analysis for voice messages.
- `internal/mcpserver` — the MCP tool definitions.
