# submux

A local relay that lets one Claude Code session run its four subagent tiers
(fable, opus, sonnet, haiku) on four different backends. It dispatches on the
model id already present in each request body, and it never substitutes one
model for another unless you explicitly configure a fallback.

- [Quick start](#quick-start)
- [Everyday use: `sc`](#everyday-use-sc)
- [How it works](#how-it-works)
- [Configuration](#configuration)
- [Commands](#commands)
- [Launcher and picker](#launcher-and-picker)
- [Troubleshooting](#troubleshooting)
- [Limits and caveats](#limits-and-caveats)

## Quick start

Requires Go 1.27+ and Claude Code. The relay (`serve`, `routes`, `check`,
`models`, `status`) is standard library only. Only `submux pick` pulls in
`bubbletea`, `bubbles` and `lipgloss`, for its TUI.

```sh
go build -o ~/bin/submux .                    # build the relay
mkdir -p ~/.config/submux
cp config.example.json ~/.config/submux/config.json   # edit routes and auth
ln -s "$PWD/bin/submux-claude" ~/bin/submux-claude
ln -s "$PWD/bin/sc" ~/bin/sc                  # short front door, needs jq

submux check claude-sonnet-5-5                # which route and subscription pays for an id
sc -p                                         # pick models in the TUI and launch Claude Code
```

The launcher starts `submux serve` on the configured port if nothing is
listening, so you do not run the relay by hand. To run it detached yourself:

```sh
nohup submux serve --listen 127.0.0.1:8787 >> ~/.local/state/submux/submux.log 2>&1 &
```

## Everyday use: `sc`

`bin/sc` is the short front door to `submux-claude`. Plain `claude` is never
touched, so keep using it for normal sessions.

| Command | Does |
|---|---|
| `sc` | Relaunch the last setup |
| `sc -p` | Open the picker |
| `sc sol` | Launch the saved setup whose alias, name or main model matches `sol` (most recently used wins) |
| `sc -l` | List saved setups |
| `sc --alias NAME ALIAS` | Name a setup (the picker's `a` key does the same) |

Anything after the word goes to `claude`, for example `sc sol --resume`.

Safety behavior built in:

- **No silent fallbacks.** Keep `default_fallback` and per-route `fallback` at
  `[]`. A failing model then returns its real error instead of another model
  answering under the picked name.
- **Launch guard.** Before `claude` starts, the main model gets a 1-token probe
  (6s cap, a pass is cached for 5 minutes). A dead login or exhausted quota
  refuses the launch, or returns you to the picker if you chose from it.
  `SUBMUX_SKIP_PREFLIGHT=1` skips it.
- **Picker.** Models whose subscription is down or out of quota are greyed out
  with the reason and cannot be chosen. Per-model probes grey single dead
  models. Enter on a dead subscription asks before opening its configured free
  fallback. A subscription can opt out of per-model probing with
  `skip_model_probe` in `subscription_probes` (use it where every request is billed).
- **Status line.** The launcher adds a status line (`submux statusline`) that
  shows the model the relay really sent each request to, per session, and turns
  loud on an error or a fallback. It is skipped when you have your own
  `statusLine`; `SUBMUX_NO_STATUSLINE=1` turns it off.
- **Hot reload.** `serve` re-reads `config.json` within 2s of a change, or on
  `SIGHUP`. A bad edit is logged and ignored. A changed `listen` needs a restart.
- **Launch warnings.** A cliproxy login that stopped refreshing, and
  `--resume` or `--continue`, print a warning and continue.

## How it works

Claude Code picks a subagent's model from the `ANTHROPIC_DEFAULT_FABLE_MODEL`,
`ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL` and
`ANTHROPIC_DEFAULT_HAIKU_MODEL` environment variables (each has a `_NAME` twin
used for display). Those four ids are the only routing signal a relay behind
`ANTHROPIC_BASE_URL` ever sees, as the `model` field of the outgoing JSON body.

With `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` both unset, Claude Code
authenticates to `ANTHROPIC_BASE_URL` with its own subscription OAuth bearer.
Setting either variable suppresses that bearer. A relay that forwards the
header untouched to `api.anthropic.com` looks, from Anthropic's side, like the
CLI talking to it directly. So `claude-*` subagents keep running on your own
subscription, and every other model id is routed, with its own credential, to
whatever API-compatible aggregator you already run.

```
claude  ->  submux :8787  ->  api.anthropic.com        (claude-*, passthrough auth)
                          ->  aggregator :8317         (everything else, own credential)
```

submux does not talk to any provider on its own. It only relays requests the
CLI was already about to send.

**Out of scope:** format translation between API schemas (your aggregator's
job), account rotation, a usage dashboard, request caching, an install script.

## Configuration

Default path: `~/.config/submux/config.json` (override with `--config`). Start
from `config.example.json`:

```json
{
  "listen": "127.0.0.1:8787",
  "max_body_bytes": 67108864,
  "default_fallback": [],
  "fallback_status_codes": [429, 402, 403, 500, 502, 503, 529],
  "cooldown_default_seconds": 300,
  "routes": [
    {
      "match": "claude-*",
      "upstream": "https://api.anthropic.com",
      "auth": "passthrough",
      "fallback": []
    },
    {
      "match": "*",
      "upstream": "http://127.0.0.1:8317",
      "auth": "bearer:keychain:local-aggregator/API_TOKEN",
      "fallback": []
    }
  ]
}
```

### Routes

`match` is a glob over the whole model id. `*` matches any run of characters
(including `/`, since some aggregators use ids like `vendor/model`) and `?`
matches exactly one. Routes are tried in order and the first match wins, so a
bare `"*"` route must be last.

`auth` is one of:

| Value | Sends |
|---|---|
| `passthrough` | The caller's own credential, untouched. Use only for a route you trust with your subscription. |
| `bearer:env:VAR` | `Authorization: Bearer $VAR` |
| `bearer:keychain:<service>/<account>` | A credential read once at startup from the macOS login keychain. A missing entry is a startup error, not a silent 401. |
| `x-api-key:env:VAR` | `x-api-key: $VAR` instead of `Authorization` |
| `none` | No credential |

Per-route options:

| Key | Meaning |
|---|---|
| `model_rewrite` | Map applied to the body's `model` field for that route. The only case where the body is re-serialized; every other route forwards the original bytes. |
| `fallback` | Ordered list of model ids to try when the upstream returns a status in `fallback_status_codes` and no response byte has reached the client. **Absent** inherits `default_fallback`. **Present and empty** (`[]`) means never fall back, fail loud. The two differ on purpose, so an explicit opt-out is never re-enabled. Each entry is re-resolved from the top of the route table, and no id is tried twice per request. |
| `subscription` | A fixed "which subscription pays" label. `check` and `models` print it with no upstream lookup. The only way to answer for a `passthrough` route. |
| `models` | Fixed model ids the picker's create wizard offers for that `subscription`. Without it the wizard shows one unselectable explanatory line. |

### Top-level options

| Key | Meaning |
|---|---|
| `listen` | Address to bind. A change needs a restart. |
| `max_body_bytes` | Request body cap. |
| `default_fallback` | Chain for any route with no `fallback` key. Applies to every request, including the main-loop session. |
| `fallback_status_codes` | Statuses that trigger a fallback. Default `429, 402, 403, 500, 502, 503, 529`. |
| `cooldown_default_seconds` | How long an id that triggered a fallback is skipped (default `300`), unless the upstream's `Retry-After` says otherwise. In memory only, reset on restart. |
| `subscriptions` | Map from an upstream `/v1/models` `owned_by` value to a subscription name. An unmapped `owned_by` prints raw, tagged `(unmapped)`. |
| `subscription_overrides` | Ordered `{"match": "<glob>", "subscription": "<name>"}` list checked before `subscriptions`. Corrects aggregators whose `owned_by` names the wire protocol, not the payer. |
| `subscription_probes` | List of `{"subscription", "model", "fallback", "skip_model_probe"}` entries. `model` is the cheap id the launch guard and picker probe to decide whether that subscription is alive. `fallback` names the subscription the picker offers (after a confirm prompt) when it has ended. `skip_model_probe: true` stops per-model probing when its list opens; set it where every request is billed. |
| `no_model_route` | Route (by its `match`) for requests with no `model` field or a non-JSON body. These are Anthropic control calls, not completions. Absent means the first `passthrough` route, or the `"*"` route if none. An unknown value fails config load and lists the valid matches. Such requests log as `model="" (no-model -> <glob>)`. |

### Fallback visibility

If you do configure a fallback, it announces itself in three places:

1. The server log: `⚠ FELL BACK <from> → <to> (...)`, and
   `✗ CHAIN EXHAUSTED <requested> → [...]` if every hop fails.
2. The response's `model` field (and a streaming `message_start` event's
   `message.model`), rewritten to the model that actually answered.
3. An `x-submux-fallback: <full chain>` response header.

Once any response byte has reached the client, submux never retries, so a
partial answer is never spliced with a second model's output.

## Commands

```sh
submux serve [--config PATH] [--listen ADDR] [--debug-headers]
submux routes [--config PATH]                  # resolved route table
submux check <model-id> [--config PATH]        # route, upstream and paying subscription
submux models [--config PATH]                  # every servable id, grouped by subscription
submux status [--config PATH] [--listen ADDR]  # cooling ids and last 20 fallbacks
submux pick [--out FILE]                       # interactive setup picker
submux statusline                              # status line for Claude Code (reads stdin JSON)
```

- **`check`** makes one upstream `/v1/models` call for a route with no fixed
  `subscription`, applies `subscription_overrides` then `subscriptions`, and
  prints the raw `owned_by` next to the mapped name. An id missing from the
  catalogue prints `NOT SERVED` plus up to 5 near-miss suggestions and exits 1.
  An unreachable upstream or missing credential prints `UNKNOWN` and exits 2.
- **`models`** fetches each non-fixed-label catalogue once and answers "what can
  I use today" in one line per subscription.
- **`status`** queries the running server's admin endpoint (fallback state is
  in that process's memory only). A fresh server prints `cooling: none` and
  `recent fallbacks: none`.

## Launcher and picker

```sh
bin/submux-claude --fable <id> --opus <id> --sonnet <id> --haiku <id> [--port N] [-- <claude args>]
```

Every tier flag is optional; an omitted tier is left unset so Claude Code uses
its own default. The launcher unsets `ANTHROPIC_API_KEY` and
`ANTHROPIC_AUTH_TOKEN` (they suppress the OAuth bearer), starts `submux serve`
if nothing listens on the port, exports `ANTHROPIC_BASE_URL` and the four tier
variables, and execs `claude`. A script that passes explicit tier flags skips
the picker entirely.

With no tier flags it launches `submux pick`: a full-screen picker (rendered to
`/dev/tty`, never stdout) with your saved setups sorted most-used-first, or a
create wizard where every step is pick-a-subscription then pick-a-model, with
no id typing.

| File | Holds |
|---|---|
| `~/.config/submux/profiles.json` | Saved setups: name, models, effort, uses, last used, alias |
| `~/.config/submux/models-cache.json` | Live model list (ids and `owned_by` only, no credential), refreshed after 10 minutes |
| `~/.local/state/submux/submux.log` | Relay log when started by the launcher |

Missing or corrupt files degrade to an empty history, not a crash. Run
`submux pick --out <file>` to inspect the `SUBMUX_MAIN`, `SUBMUX_FABLE`,
`SUBMUX_OPUS`, `SUBMUX_SONNET`, `SUBMUX_HAIKU` and `SUBMUX_PROFILE` lines it
writes.

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| Launch refused with a login or 401/403 message | The upstream login expired. Re-login in your aggregator (for Codex via cliproxy: `cliproxyapi -codex-login`), then retry. |
| Launch refused with a bare 502 | Often exhausted quota behind the aggregator. Read the aggregator's own log for the real reason. |
| A model is greyed in the picker | The probe found its subscription down or out of quota. The reason is shown on the row; press `r` to re-probe. |
| Answers come from a different model than you picked | A fallback is configured. Set `default_fallback` and every route `fallback` to `[]`. The status line and `x-submux-fallback` header show the real model. |
| Startup feels slow | The launch guard's probe is capped at 6s and cached for 5 minutes. Check `sc` output for a timeout warning. |
| Config edit has no effect | A bad edit is logged and ignored. Check `~/.local/state/submux/submux.log`. A changed `listen` needs a restart. |
| Port already in use | Another relay is running. `submux status` talks to it; `pkill -x submux` stops it. |
| Status line missing | You have your own `statusLine`, or `SUBMUX_NO_STATUSLINE=1` is set. |

## Limits and caveats

- This relies on the current, **undocumented** behavior of Claude Code's
  subscription OAuth flow (which headers it sends when, and how the four
  `ANTHROPIC_DEFAULT_*_MODEL` variables are read). It may change or stop
  working without notice in a future CLI release.
- You are responsible for complying with your provider's terms of service.
  submux does not modify, disguise or rotate credentials; it only routes
  requests you already asked the CLI to send.
- Passthrough forwards your subscription credential to whatever upstream that
  route names. Only point a `passthrough` route at a provider you trust with it.
- Probes cost real (tiny) requests. Set `skip_model_probe` in `subscription_probes` for per-request billing.
