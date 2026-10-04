# model-router

A Claude Code plugin that drives submux from inside a normal `claude` session.
You switch model, provider and effort for the main loop, each subagent tier and
each subagent type with `/model`, with no separate launcher.

## Setup

1. Run `submux serve` (see the top-level README) and point Claude Code at it in
   `~/.claude/settings.json`:

   ```json
   { "env": { "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787" } }
   ```

2. Load the plugin: add this directory to `CLAUDE_CODE_PLUGIN_DIRS`
   (colon-separated) in the same `env` block.
3. Optional: write your own routing guide at `~/.config/submux/routing-guide.md`.
   The plugin adds it to every session's prompt. `routing-guide.example.md` is a
   starting point.
4. Run the tests: `claude plugin test claude-plugin/model-router`.

## What it does

- **Live catalogue.** The plugin reads `submux models` (every id each upstream
  serves today, grouped by subscription) and keeps the native `/model` picker
  equal to it. Nothing is hardcoded.
- **Routes.** A route sets a model, an effort and an optional fallback chain for
  one tier (`fable`, `opus`, `sonnet`, `haiku`) or one subagent type.
- **Frozen per spawn.** A subagent's route is decided once, when it spawns.
  Later changes reach new spawns only, unless you add `--now`.
- **Per-spawn tag.** An Agent description that ends with `@MODEL:EFFORT`
  (for example `fix fee rounding @gpt-6.1-sol:medium`) routes that one spawn.
- **Visible.** Each Agent row shows the real model, effort, provider and who
  picked it. `/model agents` lists every subagent of the session.
- **Pick log.** One JSON line per finished subagent goes to
  `~/.local/state/model-router/picks.jsonl`.
- **Who decides.** The model may set subagent routes only. The main loop, the
  tiers and profiles stay yours.

## Commands

```
/model                          built-in picker for the main loop
/model ID                       main loop to ID (built-in)
/model routes                   routes table plus live models
/model list                     model ids by provider (the id picks the provider)
/model agents                   every subagent this session: status, real model, effort, provider, who picked
/model sonnet ID [LVL]          a tier: fable, opus, sonnet, haiku, nomodel; or an agent type (coder ID high)
/model sonnet LVL | sonnet off  effort only | clear the rule
/model --model ID --effort LVL  main loop through submux, as a route
/model --agent Explore=ID[:LVL] one subagent type (ID may be empty: --agent coder=:high)
/model --profile NAME           load a saved profile     --save NAME / --delete NAME
/model --default                save current as default for new sessions
/model --fallback TARGET=ID,ID  try these ids in order when a request fails (TARGET: slot or agent type)
/model ... --now                also switch running subagents (default: new spawns only)
/model --reset
```

A model or effort change re-reads the whole context once, because the prompt
cache is per model.
