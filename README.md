# submux

A local relay that lets one Claude Code session run its four subagent tiers
(fable, opus, sonnet, haiku) on four different backends, by dispatching on
the model id already present in each request body.

## Mechanism

Claude Code picks a subagent's model from the `ANTHROPIC_DEFAULT_FABLE_MODEL`,
`ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`, and
`ANTHROPIC_DEFAULT_HAIKU_MODEL` environment variables (each with a `_NAME`
twin used for display). Those four ids are the only routing signal a relay
sitting behind `ANTHROPIC_BASE_URL` ever sees; they arrive as the `model`
field of the outgoing JSON request body.

With `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` both unset, Claude Code
authenticates to whatever `ANTHROPIC_BASE_URL` points at using its own
subscription OAuth bearer (an `Authorization: Bearer sk-ant-o...` header).
Setting either env var suppresses that bearer. A relay that forwards this
header untouched to `api.anthropic.com` is indistinguishable, from
Anthropic's point of view, from the CLI talking to it directly — so
`claude-*` subagents keep running on your own Anthropic subscription, while
every other model id is routed (with its own separate credential) to
whatever other API-compatible aggregator you already run for the rest of
your subscriptions. submux does not talk to Anthropic or any other provider
on its own; it only relays requests the CLI was already about to send.

## What it does not do

No format translation between API schemas, no account rotation or
failover, no usage dashboard, no request caching, no config hot-reload, no
install script. If your aggregator speaks a different wire format than
Anthropic's Messages API, that translation is its job, not submux's.

## Build

Go 1.27+, standard library only, no external modules:

```sh
go build -o /usr/local/bin/submux .
```

## Configure

Default config path: `~/.config/submux/config.json` (override with
`--config`). See `config.example.json`:

```json
{
  "listen": "127.0.0.1:8787",
  "max_body_bytes": 67108864,
  "default_fallback": ["glm-5.3", "gpt-5.6-luna", "grok-4.6"],
  "fallback_status_codes": [429, 402, 403, 500, 502, 503, 529],
  "cooldown_default_seconds": 300,
  "routes": [
    {
      "match": "claude-fable-*",
      "upstream": "https://api.anthropic.com",
      "auth": "passthrough",
      "fallback": ["claude-opus-5", "gpt-5.6-luna", "glm-5.3"]
    },
    {
      "match": "claude-*",
      "upstream": "https://api.anthropic.com",
      "auth": "passthrough"
    },
    {
      "match": "*",
      "upstream": "http://127.0.0.1:8317",
      "auth": "bearer:keychain:local-aggregator/API_TOKEN"
    }
  ]
}
```

- `match` is a glob over the whole model id string: `*` matches any run of
  characters (including `/`, since real model ids from some aggregators
  contain slashes, e.g. `some-vendor/some-model`), `?` matches exactly one
  character. Routes are tried in order; the first match wins, so a bare
  `"*"` fallback route must be last.
- `auth` is one of:
  - `passthrough` — forward the caller's own credential untouched. Use this
    only for a route you trust with your own subscription credential.
  - `bearer:env:VAR` — send `Authorization: Bearer $VAR`.
  - `bearer:keychain:<service>/<account>` — read the credential once at
    startup from the macOS login keychain
    (`security find-generic-password -s <service> -a <account> -w`); a
    missing entry is a startup error, not a silent 401.
  - `x-api-key:env:VAR` — send `x-api-key: $VAR` instead of `Authorization`.
  - `none` — send no credential at all.
- `model_rewrite` (optional) — a map applied to the body's `model` field
  before forwarding, for that route only. This is the one case where the
  request body is re-serialized; every other route forwards the original
  bytes untouched.
- `fallback` (optional, per route) — an ordered, unbounded list of model ids
  to try, in order, if this route's upstream returns a status in
  `fallback_status_codes` (default `429, 402, 403, 500, 502, 503, 529`) and
  no response byte has reached the client yet. **Absent** means "inherit
  `default_fallback`"; **present and empty** (`"fallback": []`) means "never
  fall back on this route, fail loud" — the two are deliberately different,
  so an owner's explicit opt-out is never silently re-enabled. Each chain
  entry is a model id, re-resolved from the top of the route table (so it
  picks up whatever upstream, credential and `model_rewrite` that id
  normally uses); no id is ever retried twice for the same request, even if
  it appears in its own chain.
- `default_fallback` (optional, top-level) — the fallback chain used by any
  route that has no `fallback` key of its own. Applies to every request,
  including your main-loop session, not just subagents.
- `cooldown_default_seconds` (optional, top-level, default `300`) — how long
  a model id that just triggered a fallback status is skipped entirely on
  later requests, unless the upstream's own `Retry-After` says otherwise.
  Cooldowns are in-memory only and reset on restart.

### Fallback visibility

A silent substitution would mean reading output believing it came from the
model you asked for. Every fallback announces itself in three places: a
`⚠ FELL BACK <from> → <to> (...)` line in the server log (and
`✗ CHAIN EXHAUSTED <requested> → [...]` if every hop fails), the response's
`model` field (and, for a streaming response, the `message_start` event's
`message.model`) rewritten to the model that actually answered, and an
`x-submux-fallback: <full chain>` response header. Once any response byte
has reached the client, submux never retries — a partial answer is never
spliced with a second model's output.

## Run

```sh
submux serve [--config PATH] [--listen ADDR] [--debug-headers]
submux routes [--config PATH]           # print the resolved route table
submux check <model-id> [--config PATH] # which route/upstream an id would take
submux status [--config PATH] [--listen ADDR] # cooling ids + last 20 fallbacks
```

`submux status` queries a running `submux serve` process's admin endpoint
(fallback state lives in that process's memory only, never on disk), and
prints which model ids are currently cooling and the last 20 fallback
events. Run it against a fresh server with no fallbacks yet and it prints
`cooling: none` / `recent fallbacks: none`.

## Launch Claude Code through it

```sh
bin/submux-claude --fable <id> --opus <id> --sonnet <id> --haiku <id> [--port N] [-- <claude args>]
```

Every tier flag is optional; an omitted tier is left unset so Claude Code
falls back to its own default for that tier. The launcher unsets
`ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` (they suppress the OAuth bearer
this tool depends on), starts `submux serve` if nothing is already
listening on the configured port, exports `ANTHROPIC_BASE_URL` and the four
tier env vars, and execs `claude`.

## Caveats

- This relies on the current, **undocumented** behaviour of Claude Code's
  subscription OAuth flow (which headers it sends when, and how the four
  `ANTHROPIC_DEFAULT_*_MODEL` variables are read). It may change or stop
  working without notice in a future CLI release.
- You are responsible for complying with your own provider(s)' terms of
  service. submux does not modify, disguise, or rotate credentials; it only
  routes requests you already asked the CLI to send.
- Passthrough forwards your subscription credential to whatever upstream
  that route names — only point a `passthrough` route at a provider you
  trust with it.
