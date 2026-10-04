// Pure routing logic: no `$`, so tests import it directly.
import type { Effort, Provider, Routes, Rule, Seen, Slot, Spawn, Who } from '../types'

export const SLOTS: readonly Slot[] = ['main', 'inherit', 'fable', 'opus', 'sonnet', 'haiku']
export const TIERS: readonly Slot[] = ['fable', 'opus', 'sonnet', 'haiku']
export const EFFORTS: readonly Effort[] = ['low', 'medium', 'high', 'xhigh', 'max']
export const EMPTY: Routes = { slots: {}, agents: {} }

/** `submux models` prints `Provider (N ids): a, b, c` per line. */
export function parseCatalogue(text: string): Provider[] {
  const out: Provider[] = []
  for (const line of text.split('\n')) {
    const m = line.match(/^(.*) \(\d+ ids\): (.*)$/)
    if (m) out.push({ name: (m[1] ?? "").trim(), ids: (m[2] ?? "").split(',').map(s => s.trim()).filter(Boolean) })
  }
  return out
}

export function providerOf(catalogue: readonly Provider[], id: string): string | undefined {
  return catalogue.find(p => p.ids.includes(id))?.name
}

/** Which tier a resolved model id belongs to, from its family name. */
export function tierOf(model: string): Slot | undefined {
  const m = model.match(/(fable|opus|sonnet|haiku)/)
  return m ? (m[1] as Slot) : undefined
}

export type Step = { model: string; effort?: Effort | number; agentId?: string; fallback?: string[] }

/**
 * The model and effort one request goes out with. Each field falls through
 * independently: agent type, then tier (or inherit, then main for a subagent
 * running the session's own model), then the engine's own value.
 */
export function resolve(routes: Routes, step: Step, agentType: string | undefined, sessionModel: string): Step {
  const chain: (Rule | undefined)[] = []
  if (!step.agentId) chain.push(routes.slots.main)
  else {
    // A subagent with no spawn record (spawned before this mod loaded). Equal to the
    // session model does not mean inheriting: cards pin the same model with their own effort.
    if (agentType) chain.push(routes.agents[agentType])
    const tier = tierOf(step.model)
    if (tier) chain.push(routes.slots[tier])
    void sessionModel
  }
  const pick = <K extends keyof Rule>(k: K) => chain.find(r => r?.[k] !== undefined)?.[k]
  const model = pick('model') ?? step.model
  const effort = pick('effort') ?? step.effort
  const fallback = pick('fallback')?.filter(id => id !== model)
  return { ...step, model, ...(effort === undefined ? {} : { effort }), ...(fallback?.length ? { fallback } : {}) }
}

/** `fix fee @gpt-6.1-sol:medium`: the session's per-spawn route, stripped from the description. */
export function parseTag(description: string): { description: string; model?: string; effort?: Effort } {
  const m = description.match(/^(.*?)(?:^|\s+)@([^\s@:]+)?(?::([a-z]+))?\s*$/)
  if (!m || (!m[2] && !m[3])) return { description }
  const effort = EFFORTS.includes(m[3] as Effort) ? (m[3] as Effort) : undefined
  return { description: m[1]!.trim() || description, ...(m[2] ? { model: m[2] } : {}), ...(effort ? { effort } : {}) }
}

/** `model:` and `effort:` from an agent card's frontmatter. */
export function parseCard(text: string): { model?: string; effort?: Effort } {
  const fm = text.match(/^---\n([\s\S]*?)\n---/)?.[1] ?? ''
  const get = (k: string) => fm.match(new RegExp(`^${k}:\\s*["']?([^"'\\s#]+)`, 'm'))?.[1]
  const model = get('model')
  const effort = get('effort')
  return { ...(model && model !== 'inherit' ? { model } : {}), ...(EFFORTS.includes(effort as Effort) ? { effort: effort as Effort } : {}) }
}

/** `retired: a, b` lines in the routing guide. */
export function retiredIds(guide: string): string[] {
  return guide.split('\n').flatMap(l => l.match(/^\s*-?\s*retired:\s*(.*)$/i)?.[1]?.split(',').map(s => s.trim()).filter(Boolean) ?? [])
}

/**
 * Routes the user typed in a chat message: `review-agent=gpt-6.1-sol:medium`, `coder=:high`,
 * `sonnet=kiro/claude-sonnet-5.5`. Only names that are a slot or a known agent type,
 * and only ids in the catalogue (or an effort alone), so code like `x=1` never matches.
 */
export function routesInText(text: string, agentTypes: readonly string[], catalogue: readonly Provider[]): string[] {
  const ids = new Set(catalogue.flatMap(p => p.ids))
  const flags: string[] = []
  for (const m of text.matchAll(/(?:^|\s)([A-Za-z][\w-]*)=([^\s:,;]*)(?::(low|medium|high|xhigh|max))?(?=[\s,;.]|$)/g)) {
    const [, name, id, effort] = m as unknown as [string, string, string, string | undefined]
    if (id && !ids.has(id)) continue
    if (!id && !effort) continue
    if ((SLOTS as readonly string[]).includes(name)) {
      if (id) flags.push(`--${name} ${id}`)
      if (effort) flags.push(`--${name}-effort ${effort}`)
    } else if (agentTypes.includes(name)) flags.push(`--agent ${name}=${id}${effort ? `:${effort}` : ''}`)
  }
  return flags
}

/**
 * What the session may set through its tool: subagent rules only, never main or a
 * tier, never a field the user set. Returns the merged routes or the refusal.
 */
export function sessionSet(cur: Routes, args: string): { routes?: Routes; error?: string } {
  const p = parseArgs(args, EMPTY)
  if (p.errors.length) return { error: p.errors.join('; ') }
  if (Object.keys(p.routes.slots).length || Object.keys(p.actions).some(k => k !== 'now')) {
    return { error: 'the session may set subagent routes only (--agent, --fallback); main, tiers and profiles are the user\'s' }
  }
  const agents = { ...cur.agents }
  for (const [type, r] of Object.entries(p.routes.agents)) {
    const have = cur.agents[type]
    if (have && !have.by) return { error: `${type} route was set by the user (${ruleText(have)}); ask the user to change it` }
    agents[type] = { ...have, ...r, by: 'session' }
  }
  return { routes: { ...cur, agents } }
}

/** Retired on any provider and context size: `kiro/claude-opus-5[1m]` matches `claude-opus-5`. */
export function isRetired(id: string, retired: readonly string[]): boolean {
  const bare = id.replace(/\[1m\]$/, '')
  return retired.some(r => bare === r || bare.endsWith(`/${r}`))
}

export type SpawnIn = {
  routes: Routes; type: string; tag: { model?: string; effort?: Effort }; card: { model?: string; effort?: Effort }
  param?: string; parentModel: string
}

/**
 * A subagent's route, decided once at spawn. Per field: your agent-type rule, the
 * session's @tag, the session's agent-type rule, then the agent's own choice (Agent
 * `model` param, card), mapped through your tier map. Only an agent with no model
 * and no effort of its own follows `inherit`, then main: a card effort never leaks.
 */
export function resolveSpawn(i: SpawnIn): { model: string; effort?: Effort; fallback?: string[]; by: { model: Who; effort: Who } } {
  const rule = i.routes.agents[i.type]
  const mine = rule && !rule.by ? rule : undefined
  const theirs = rule?.by === 'session' ? rule : undefined
  const own = i.param ?? i.card.model
  const inherits = !own && !i.card.effort
  let model: string, mBy: Who
  if (mine?.model) [model, mBy] = [mine.model, 'you']
  else if (i.tag.model) [model, mBy] = [i.tag.model, 'session']
  else if (theirs?.model) [model, mBy] = [theirs.model, 'session']
  else if (own) [model, mBy] = [own, 'engine']
  else if (i.routes.slots.inherit?.model) [model, mBy] = [i.routes.slots.inherit.model, 'you']
  else [model, mBy] = [i.routes.slots.main?.model ?? i.parentModel, 'main']
  const tier = mBy === 'engine' || mBy === 'main' ? tierOf(model) : undefined
  const tierRule = tier ? i.routes.slots[tier] : undefined
  if (tierRule?.model) [model, mBy] = [tierRule.model, 'tier']
  let effort: Effort | undefined, eBy: Who = 'engine'
  if (mine?.effort) [effort, eBy] = [mine.effort, 'you']
  else if (i.tag.effort) [effort, eBy] = [i.tag.effort, 'session']
  else if (theirs?.effort) [effort, eBy] = [theirs.effort, 'session']
  else if (i.card.effort) [effort, eBy] = [i.card.effort, 'card']
  else if (tierRule?.effort) [effort, eBy] = [tierRule.effort, 'tier']
  else if (inherits && i.routes.slots.inherit?.effort) [effort, eBy] = [i.routes.slots.inherit.effort, 'you']
  else if (inherits && i.routes.slots.main?.effort) [effort, eBy] = [i.routes.slots.main.effort, 'main']
  const fallback = (mine?.fallback ?? theirs?.fallback ?? tierRule?.fallback)?.filter(id => id !== model)
  return { model, ...(effort ? { effort } : {}), ...(fallback?.length ? { fallback } : {}), by: { model: mBy, effort: eBy } }
}

/** The compact spawn-row and band line. */
export function spawnLine(s: Spawn, catalogue: readonly Provider[], quota?: Record<string, number>): string {
  const real = s.real ?? s.model
  const prov = s.provider ?? providerOf(catalogue, real)
  const q = prov && quota?.[prov] !== undefined ? ` ${quota[prov]}%` : ''
  const by = s.by.model === s.by.effort ? s.by.model : `${s.by.model}/${s.by.effort}`
  const parts = [`${real}/${s.effort ?? 'default'}`, prov ? `${prov}${q}` : undefined, `by ${by}`]
  if (s.trail.length) parts.push(`trail ${s.trail.join(' → ')}`)
  if (s.tokens) parts.push(`${Math.round(s.tokens / 1000)}k tok`)
  return parts.filter(Boolean).join(' · ')
}

/** The hint-line tail for one subagent: `grok-4.7-build/med (SuperGrok)`. */
export function tailLine(s: Spawn, catalogue: readonly Provider[]): string {
  const prov = s.provider ?? providerOf(catalogue, s.real ?? s.model)
  const effort = s.effort ? `/${s.effort === 'medium' ? 'med' : s.effort}` : ''
  return `${s.real ?? s.model}${effort}${prov ? ` (${shortProvider(prov)})` : ''}`
}

/** One line for a subagent (or main): what it really ran on. */
export function seenLine(s: Seen): string {
  const parts = [s.model, s.effort ?? 'default effort', s.provider].filter(Boolean)
  const fb = s.fellBackFrom ? ` (fallback from ${s.fellBackFrom})` : ''
  return `${parts.join(' · ')}${fb}`
}

export function isEmpty(routes: Routes): boolean {
  return Object.values(routes.slots).every(r => !r || (r.model === undefined && r.effort === undefined && !r.fallback?.length)) && Object.keys(routes.agents).length === 0
}

function ruleText(r: Rule | undefined): string {
  const t = [r?.model, r?.effort].filter(Boolean).join('/')
  return r?.fallback?.length ? `${t}>${r.fallback.join('>')}` : t
}

/** Short status-line text; undefined when nothing is overridden. */
export function statusText(routes: Routes): string | undefined {
  if (isEmpty(routes)) return undefined
  const parts: string[] = []
  for (const s of SLOTS) {
    const t = ruleText(routes.slots[s])
    if (t) parts.push(`${s}:${t}`)
  }
  for (const [a, r] of Object.entries(routes.agents)) {
    const t = ruleText(r)
    if (t) parts.push(`@${a}:${t}`)
  }
  return `⇄ ${parts.join(' · ')}`
}

/** Plain-text table of the current routing, after a `/model --flag` change. */
export function describe(routes: Routes, catalogue: readonly Provider[]): string {
  const row = (name: string, r: Rule | undefined) => {
    const prov = r?.model ? providerOf(catalogue, r.model) ?? 'unknown provider' : ''
    return `${name.padEnd(16)} ${(r?.model ?? '(launch default)').padEnd(30)} ${(r?.effort ?? '(default)').padEnd(10)} ${prov}${r?.fallback?.length ? `  fallback: ${r.fallback.join(' > ')}` : ''}`
  }
  const lines = SLOTS.map(s => row(s, routes.slots[s]))
  for (const [a, r] of Object.entries(routes.agents)) lines.push(row(`agent ${a}`, r))
  return lines.join('\n')
}

/** A main model with a context window other than the launch model's. */
export function contextWarning(routes: Routes, sessionModel: string): string | undefined {
  const m = routes.slots.main?.model
  if (!m || m === sessionModel) return undefined
  const big = (id: string) => id.endsWith('[1m]')
  if (big(m) !== big(sessionModel) || !/claude-/.test(m)) {
    return `Context is still counted as ${sessionModel}. If ${m} has a smaller window, run /compact early.`
  }
  return undefined
}

export type Parsed = {
  routes: Routes
  actions: { show?: boolean; list?: boolean; agents?: boolean; now?: boolean; saveDefault?: boolean; saveProfile?: string; loadProfile?: string; deleteProfile?: string }
  errors: string[]
}

const OFF = new Set(['off', 'default', 'none', '-'])

function setField(routes: Routes, target: { slot?: Slot; agent?: string }, field: keyof Rule, value: string, errors: string[]): Routes {
  if (field === 'effort' && !OFF.has(value) && !EFFORTS.includes(value as Effort)) {
    errors.push(`effort must be one of ${EFFORTS.join(', ')} or off, got ${value}`)
    return routes
  }
  const cur = target.slot ? routes.slots[target.slot] : routes.agents[target.agent!]
  const next: Rule = { ...cur }
  delete next.by // a rule the user touches becomes the user's
  if (OFF.has(value)) delete next[field]
  else if (field === 'fallback') next.fallback = value.split(',').map(s => s.trim()).filter(Boolean)
  else (next as Record<string, string>)[field] = value
  if (target.slot) return { ...routes, slots: { ...routes.slots, [target.slot]: next } }
  const agents = { ...routes.agents, [target.agent!]: next }
  if (next.model === undefined && next.effort === undefined && !next.fallback?.length) delete agents[target.agent!]
  return { ...routes, agents }
}

/**
 * Flags in `claude` style: `--model X --effort Y`, `--sonnet X --sonnet-effort Y`
 * (any slot), `--agent Explore=MODEL[:EFFORT]`, `--profile NAME`, `--save NAME`,
 * `--delete NAME`, `--default`, `--reset`, `--show`, `--list`. `off` clears a field.
 */
export function parseArgs(args: string, start: Routes): Parsed {
  const t = args.trim().split(/\s+/).filter(Boolean)
  let routes = start
  const actions: Parsed['actions'] = {}
  const errors: string[] = []
  if (t.length === 0) return { routes, actions: { show: true }, errors }
  for (let i = 0; i < t.length; i++) {
    const flag = t[i]!
    const val = () => {
      const v = t[++i]
      if (v === undefined || v.startsWith('--')) {
        errors.push(`${flag} needs a value`)
        if (v !== undefined) i--
        return undefined
      }
      return v
    }
    const m = flag.match(/^--(main|inherit|fable|opus|sonnet|haiku)(-effort)?$/)
    if (flag === '--model' || flag === '--effort') {
      const v = val()
      if (v) routes = setField(routes, { slot: 'main' }, flag === '--model' ? 'model' : 'effort', v, errors)
    } else if (m) {
      const v = val()
      if (v) routes = setField(routes, { slot: m[1] as Slot }, m[2] ? 'effort' : 'model', v, errors)
    } else if (flag === '--agent') {
      const v = val()
      const am = v?.match(/^([^=]+)=([^:]*)(?::(.+))?$/)
      if (v && !am) errors.push(`--agent wants TYPE=MODEL[:EFFORT], got ${v}`)
      if (am) {
        if (am[2]) routes = setField(routes, { agent: am[1] }, 'model', am[2], errors)
        if (am[3]) routes = setField(routes, { agent: am[1] }, 'effort', am[3], errors)
      }
    } else if (flag === '--fallback') {
      const v = val()
      const fm = v?.match(/^([^=]+)=(.+)$/)
      if (v && !fm) errors.push(`--fallback wants TARGET=ID,ID (TARGET a slot or agent type), got ${v}`)
      if (fm) {
        const name = fm[1]!
        const target = (SLOTS as readonly string[]).includes(name) ? { slot: name as Slot } : { agent: name }
        routes = setField(routes, target, 'fallback', fm[2]!, errors)
      }
    } else if (flag === '--reset') routes = EMPTY
    else if (flag === '--profile') actions.loadProfile = val()
    else if (flag === '--save') actions.saveProfile = val()
    else if (flag === '--delete') actions.deleteProfile = val()
    else if (flag === '--default') actions.saveDefault = true
    else if (flag === '--show') actions.show = true
    else if (flag === '--list') actions.list = true
    else if (flag === '--agents') actions.agents = true
    else if (flag === '--now') actions.now = true
    else if (!flag.startsWith('--') && i === 0 && t.length === 1) actions.loadProfile = flag
    else errors.push(`unknown flag ${flag}`)
  }
  return { routes, actions, errors }
}

/** A submux-claude profile (`~/.config/submux/profiles.json`) as routes. */
export function fromSubmuxProfile(p: { main?: string; fable?: string; opus?: string; sonnet?: string; haiku?: string; effort?: string }): Routes {
  const slots: Routes['slots'] = {}
  const eff = EFFORTS.includes(p.effort as Effort) ? (p.effort as Effort) : undefined
  if (p.main || eff) slots.main = { ...(p.main ? { model: p.main } : {}), ...(eff ? { effort: eff } : {}) }
  for (const s of TIERS) {
    const id = (p as Record<string, string | undefined>)[s]
    if (id) slots[s] = { model: id }
  }
  return { slots, agents: {} }
}

/** Ids a rule names that the catalogue does not list. */
export function unknownIds(routes: Routes, catalogue: readonly Provider[]): string[] {
  if (catalogue.length === 0) return []
  const ids = [...Object.values(routes.slots), ...Object.values(routes.agents)].map(r => r?.model).filter((x): x is string => !!x)
  return [...new Set(ids)].filter(id => !providerOf(catalogue, id))
}

/** A cliproxy login whose access token expired over a day ago (refresh likely dead). */
export function staleLogin(auth: { disabled?: boolean; expired?: string; type?: string; email?: string }, nowMs: number): string | undefined {
  if (auth.disabled || !auth.expired) return undefined
  const t = Date.parse(auth.expired)
  if (Number.isNaN(t) || nowMs - t <= 86_400_000) return undefined
  const type = auth.type ?? '?'
  return `${type} login (${auth.email ?? '?'}) stopped refreshing, expired ${auth.expired}; re-login: cliproxyapi -${type}-login`
}

/** `Kiro (kirocc :3456)` → `Kiro`. */
export function shortProvider(name: string): string {
  return name.replace(/\s*\(.*\)\s*$/, '')
}

const ROW_NAMES: Record<Slot, string> = { main: 'Main loop', inherit: 'Agents with no model', fable: 'Fable agents', opus: 'Opus agents', sonnet: 'Sonnet agents', haiku: 'Haiku agents' }
const TARGET_ALIASES: Record<string, Slot> = { main: 'main', nomodel: 'inherit', inherit: 'inherit', fable: 'fable', opus: 'opus', sonnet: 'sonnet', haiku: 'haiku' }

/**
 * `/model routes` text. A subagent's model, provider and effort are picked per spawn, so no row
 * claims one: main, then the fixed rules you set, then what recent subagents really ran on.
 */
export function routesTable(routes: Routes, catalogue: readonly Provider[], main: { model: string; effort?: string }, recent: readonly Spawn[]): string {
  const prov = (id: string) => { const p = providerOf(catalogue, id); return p ? ` · ${shortProvider(p)}` : '' }
  const ruleCell = (r: Rule) => `${r.model ?? 'model picked per spawn'}${r.effort ? ` · ${r.effort}` : ''}${r.model ? prov(r.model) : ''}`
  const pad = (rows: [string, string][]) => {
    const w = Math.max(...rows.map(r => r[0].length)) + 3
    return rows.map(([n, v]) => `  ${n.padEnd(w)}${v}`)
  }
  const m = routes.slots.main
  const mainCell = m?.model || m?.effort ? `${ruleCell(m)}   (your rule)` : `${main.model}${main.effort ? ` · ${main.effort}` : ''}${prov(main.model)}`
  const rules: [string, string][] = []
  for (const s of SLOTS) {
    const r = routes.slots[s]
    if (s !== 'main' && (r?.model || r?.effort)) rules.push([ROW_NAMES[s], ruleCell(r!)])
  }
  for (const [a, r] of Object.entries(routes.agents)) if (r.model || r.effort) rules.push([a, ruleCell(r)])
  const ran: [string, string][] = recent.map(s => {
    const real = s.real ?? s.model
    const p = s.provider ?? providerOf(catalogue, real)
    return [`${s.type} (${s.description})`, `${real} · ${s.effort ?? 'default effort'}${p ? ` · ${shortProvider(p)}` : ''} · picked by ${s.by.model}`]
  })
  return [
    `Main loop: ${mainCell}`,
    'Subagents: picked per spawn (your rule, the session\'s @MODEL tag, or the agent card)',
    '',
    'Your fixed rules for subagents',
    ...(rules.length ? pad(rules) : ['  none']),
    '',
    'Recent subagents (what really ran)',
    ...(ran.length ? pad(ran) : ['  none yet this session']),
    '',
    'Change main:       /model <id>   (bare /model opens the picker)',
    'Change subagents:  /model sonnet gpt-6.1-sol high   (or: nomodel, opus, fable, haiku, an agent type)',
    'Undo:              /model sonnet off',
    'Model ids:         /model list      More: /model help',
  ].join('\n')
}

/** `/model list`: one line per provider, short names. */
export function providersText(catalogue: readonly Provider[]): string {
  if (!catalogue.length) return 'submux catalogue is empty or submux is not running.'
  const w = Math.max(...catalogue.map(p => shortProvider(p.name).length)) + 3
  return catalogue.map(p => `${shortProvider(p.name).padEnd(w)}${p.ids.join(', ')}`).join('\n')
}

/**
 * Who serves a `/model` run. Bare and a single id stay the built-in picker for main;
 * `routes`, `list`, `agents`, `help`, any `--flag` and `<target> <model|effort|off> [effort]`
 * are the router's.
 */
export function modelArgs(args: string): 'builtin' | 'table' | 'list' | 'agents' | 'help' | 'flags' | 'short' {
  const t = args.trim().split(/\s+/).filter(Boolean)
  if (t.length === 0) return 'builtin'
  const w = t[0]!.toLowerCase()
  if (t.length === 1) {
    if (w === 'routes' || w === 'show') return 'table'
    if (w === 'list') return 'list'
    if (w === 'agents') return 'agents'
    if (w === 'help' || w === '-h') return 'help'
  }
  if (t.some(x => x.startsWith('--'))) return 'flags'
  return t.length === 1 ? 'builtin' : 'short'
}

/**
 * `/model <target> <model|off> [effort]` or `/model <target> <effort>`. A target is a tier,
 * `nomodel`, or an agent type; `main` takes only `off` (main is the built-in /model's).
 */
export function parseShort(args: string, cur: Routes, catalogue: readonly Provider[]): { routes?: Routes; error?: string } {
  const [rawTarget, a, b, ...rest] = args.trim().split(/\s+/)
  if (!rawTarget || !a || rest.length) return { error: 'use: /model <tier or agent type> <model id | off> [effort]' }
  const slot = TARGET_ALIASES[rawTarget.toLowerCase()]
  const isEffort = (x: string) => (EFFORTS as readonly string[]).includes(x)
  let rule: Rule
  if (a === 'off') rule = {}
  else if (slot === 'main') return { error: 'change the main loop with /model <id>; "/model main off" clears an old route' }
  else if (isEffort(a) && !b) rule = { ...(slot ? cur.slots[slot] : cur.agents[rawTarget]), effort: a as Effort }
  else {
    if (!catalogue.some(p => p.ids.includes(a))) return { error: `${a} is not a model id. See /model list` }
    if (b && !isEffort(b)) return { error: `${b} is not an effort. Use: ${EFFORTS.join(', ')}` }
    rule = { model: a, ...(b ? { effort: b as Effort } : {}) }
  }
  if (slot) {
    const slots = { ...cur.slots }
    if (rule.model || rule.effort) slots[slot] = rule; else delete slots[slot]
    return { routes: { ...cur, slots } }
  }
  const agents = { ...cur.agents }
  if (rule.model || rule.effort) agents[rawTarget] = rule; else delete agents[rawTarget]
  return { routes: { ...cur, agents } }
}
