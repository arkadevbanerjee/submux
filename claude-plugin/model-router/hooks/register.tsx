import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, TurnStepChunk, TurnStepResult } from 'claude-code'

import type { Effort, Provider, Routes, Rule, Seen, Slot, Spawn } from '../types'
import {
  EFFORTS, EMPTY, SLOTS, contextWarning, describe, fromSubmuxProfile, isEmpty, isRetired, parseArgs, parseCard, parseCatalogue,
  modelArgs, parseShort, parseTag, providerOf, providersText, resolve, routesTable, seenLine, spawnLine, resolveSpawn, retiredIds, routesInText, sessionSet, staleLogin, statusText, tailLine, unknownIds,
} from './route'

const routes = atom({ plugin: 'model-router', key: 'routes' } as const, EMPTY)
const catalogue = atom({ plugin: 'model-router', key: 'catalogue' } as const, [] as Provider[])
const profiles = atom({ plugin: 'model-router', key: 'profiles' } as const, {} as Record<string, Routes>)

const spawns = atom({ plugin: 'model-router', key: 'spawns' } as const, {} as Record<string, Spawn>)
const seen = atom({ plugin: 'model-router', key: 'seen' } as const, {} as Record<string, Seen>)

const GUIDE = '.config/submux/routing-guide.md'

async function guideText($: EngineInterface): Promise<string> {
  try {
    return await $.fs.read(`${(await $.env.get('HOME')) ?? ''}/${GUIDE}`)
  } catch {
    return ''
  }
}

/** Routes frozen at spawn but not yet tied to an agentId (the first step can come first). */
const pendingA = atom({ plugin: 'model-router', key: 'pending' } as const, [] as Spawn[])
const health = atom({ plugin: 'model-router', key: 'health' } as const, {} as Record<string, { ok: number; failed: number }>)

/** Failed vs ok requests per provider this session, plus submux's cooling list. */
async function healthText($: EngineInterface): Promise<string> {
  const h = await read($, health)
  const parts = Object.entries(h).map(([p, c]) => `${p} ${c.ok} ok/${c.failed} failed`)
  try {
    const r = await $.process.run(['submux', 'status'])
    const cooling = r.stdout.match(/^cooling: (.*)$/m)?.[1]
    if (cooling && cooling !== 'none') parts.push(`cooling: ${cooling}`)
  } catch {}
  return parts.join('; ') || 'no requests yet'
}

async function agentTypes($: EngineInterface): Promise<string[]> {
  const home = (await $.env.get('HOME')) ?? ''
  const out = ['general-purpose', 'Explore', 'Plan']
  try {
    for (const f of await $.fs.list(`${home}/.claude/agents`)) if (f.name.endsWith('.md')) out.push(f.name.slice(0, -3))
  } catch {}
  return out
}

const retired$ =async ($: EngineInterface) => retiredIds(await guideText($))

/** An agent card's own model and effort: user cards, then project cards. */
async function cardOf($: EngineInterface, type: string): Promise<{ model?: string; effort?: Effort }> {
  const home = (await $.env.get('HOME')) ?? ''
  const cwd = (await $.env.get('PWD')) ?? ''
  for (const dir of [`${cwd}/.claude/agents`, `${home}/.claude/agents`]) {
    try {
      return parseCard(await $.fs.read(`${dir}/${type}.md`))
    } catch {}
  }
  return {}
}

async function bump($: EngineInterface, provider: string | undefined, ok: boolean) {
  if (!provider) return
  await update($, health, h => {
    const c = h[provider] ?? { ok: 0, failed: 0 }
    return { ...h, [provider]: ok ? { ...c, ok: c.ok + 1 } : { ...c, failed: c.failed + 1 } }
  })
}

/** What one request really ran on: the answering model, tokens, the fallback trail. */
async function record($: EngineInterface, agentId: string | undefined, asked: string, effort: Effort | number | undefined, res: TurnStepResult, trail: string[]) {
  const u = (res.usage ?? {}) as { model?: string; input_tokens?: number; output_tokens?: number }
  const real = u.model ?? asked
  const provider = providerOf(await read($, catalogue), asked) ?? providerOf(await read($, catalogue), real)
  const tokens = (u.input_tokens ?? 0) + (u.output_tokens ?? 0)
  for (const id of trail.slice(0, -1)) await bump($, providerOf(await read($, catalogue), id), false)
  await bump($, provider, res.stopReason !== null)
  if (agentId) {
    await update($, spawns, all => {
      const s = all[agentId]
      if (!s) return all
      return { ...all, [agentId]: { ...s, real, ...(provider ? { provider } : {}), tokens: s.tokens + tokens, trail: trail.length ? trail : s.trail } }
    })
    return
  }
  await update($, seen, all => ({
    ...all,
    main: {
      key: 'main', type: 'main', description: 'main loop', model: real,
      ...(effort === undefined ? {} : { effort: String(effort) }), ...(provider ? { provider } : {}),
      ...(trail.length ? { fellBackFrom: trail[0] } : {}), at: Date.now(),
    },
  }))
}

const SLOT_HELP: Record<Slot, string> = {
  main: 'main loop',
  inherit: 'subagents with no model (default: same as main)',
  fable: 'fable tier',
  opus: 'opus tier',
  sonnet: 'sonnet tier',
  haiku: 'haiku tier (subagents only)',
}

async function loadCatalogue($: EngineInterface): Promise<Provider[]> {
  const home = (await $.env.get('HOME')) ?? ''
  for (const bin of ['submux', `${home}/bin/submux`]) {
    try {
      const r = await $.process.run([bin, 'models'])
      if (r.exitCode === 0) return parseCatalogue(r.stdout)
    } catch {}
  }
  return []
}

/** Saved profiles; on first use, seeded from submux-claude's profiles.json. */
async function loadProfiles($: EngineInterface): Promise<Record<string, Routes>> {
  const saved = (await $.store.get('profiles')) as Record<string, Routes> | undefined
  if (saved) return saved
  const seeded: Record<string, Routes> = {}
  try {
    const home = (await $.env.get('HOME')) ?? ''
    const raw = JSON.parse(await $.fs.read(`${home}/.config/submux/profiles.json`)) as { profiles?: ({ name?: string } & Parameters<typeof fromSubmuxProfile>[0])[] }
    for (const p of raw.profiles ?? []) if (p.name) seeded[p.name] = fromSubmuxProfile(p)
  } catch {}
  await $.store.set('profiles', seeded)
  return seeded
}

async function prelaunch($: EngineInterface) {
  const home = (await $.env.get('HOME')) ?? ''
  const path = `${home}/.config/submux/prelaunch`
  try {
    await $.fs.stat(path)
  } catch {
    return
  }
  try {
    const r = await $.process.run([path])
    if (r.exitCode !== 0) $.ui.toast(`submux prelaunch exited ${r.exitCode}: Kiro models may be stale`)
  } catch {
    $.ui.toast('submux prelaunch failed to run: Kiro models may be stale')
  }
}

async function staleLogins($: EngineInterface): Promise<string[]> {
  const home = (await $.env.get('HOME')) ?? ''
  const dir = `${home}/.cli-proxy-api`
  const out: string[] = []
  try {
    for (const f of await $.fs.list(dir)) {
      if (!f.name.endsWith('.json')) continue
      try {
        const line = staleLogin(JSON.parse(await $.fs.read(`${dir}/${f.name}`)), Date.now())
        if (line) out.push(line)
      } catch {}
    }
  } catch {}
  return out
}

async function apply($: EngineInterface, next: Routes) {
  await update($, routes, () => next)
  $.ui.status(statusText(next))
}

/**
 * /model and /effort overwrite `model` and `effortLevel` in settings.json as the default for
 * new sessions. The pin file holds the defaults the user wants; the guard restores them, so a
 * per-session /model change never sticks. First run pins whatever settings.json holds.
 */
async function guardDefaults($: EngineInterface) {
  const home = await $.env.get('HOME')
  const settingsPath = `${home}/.claude/settings.json`
  const pinPath = `${home}/.claude/pinned-defaults.json`
  const KEYS = ['model', 'effortLevel'] as const
  try {
    const settings = JSON.parse(await $.fs.read(settingsPath) as string)
    if (!(await $.fs.exists(pinPath))) {
      const pin = Object.fromEntries(KEYS.filter(k => settings[k] !== undefined).map(k => [k, settings[k]]))
      await $.fs.write(pinPath, JSON.stringify(pin, null, 2) + '\n')
      return
    }
    const pin = JSON.parse(await $.fs.read(pinPath) as string)
    const drift = KEYS.filter(k => pin[k] !== undefined && settings[k] !== pin[k])
    if (!drift.length) return
    for (const k of drift) settings[k] = pin[k]
    await $.fs.write(settingsPath, JSON.stringify(settings, null, 2) + '\n')
  } catch { /* settings or pin unreadable: leave both alone */ }
}

/** Claude Max ids the native rows already cover, and ids the routing guide retires. */
const PICKER_SKIP = new Set(['claude-opus-5', 'claude-sonnet-5', 'gpt-6-sol', 'gpt-5.6-luna', 'claude-gpt-5.6-luna'])

/**
 * Keeps settings.json `modelPicker` equal to the live submux catalogue, so the native /model
 * picker lists every model submux serves, tagged with its provider. Writes only on a change.
 */
async function syncPicker($: EngineInterface) {
  const cat = await loadCatalogue($)
  if (!cat.length) return
  const options = cat.flatMap(p => {
    const tag = p.name.replace(/ \(.*\)/, '').replace('Google AI Pro', 'Antigravity').replace('ChatGPT', 'Codex').replace(' Coding Plan', '')
    return p.ids
      .filter(id => !(id.startsWith('claude-') && !id.startsWith('claude-gpt')) && !PICKER_SKIP.has(id.split('/').pop()!))
      .map(id => ({ model: id, label: `${tag} · ${id}`, description: p.name }))
  })
  if (!options.length) return
  const home = await $.env.get('HOME')
  const settingsPath = `${home}/.claude/settings.json`
  try {
    const settings = JSON.parse(await $.fs.read(settingsPath) as string)
    if (JSON.stringify(settings.modelPicker?.options) === JSON.stringify(options)) return
    settings.modelPicker = { ...settings.modelPicker, options }
    await $.fs.write(settingsPath, JSON.stringify(settings, null, 2) + '\n')
  } catch { /* settings unreadable: leave it alone */ }
}

/** One at a time: both rewrite settings.json, so they must not interleave. */
async function tick($: EngineInterface) {
  await guardDefaults($)
  await syncPicker($)
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    void tick($)
    $.clock.every(5000, () => void guardDefaults($))
    $.clock.every(60000, () => void tick($))
    await $.tool.register({
      name: 'route',
      description: 'Set a subagent route for this session: args like "--agent coder=gpt-6.1-sol:medium" or "--fallback review-agent=kiro/claude-opus-5.5,glm-5.3". Subagent types only; main and anything the user set are refused. Prefer a per-spawn @MODEL:EFFORT tag for one spawn.',
      inputSchema: { type: 'object', properties: { args: { type: 'string' } }, required: ['args'] },
    })
    const start = ((await $.store.get('default')) as Routes | undefined) ?? EMPTY
    await apply($, start)
    // What submux-claude did before launch: prelaunch (Kiro catalogue sync, kirocc up), then the
    // stale-login check. Never blocks the session; the catalogue loads after prelaunch settles.
    void prelaunch($).then(() => loadCatalogue($)).then(c => update($, catalogue, () => c))
    void staleLogins($).then(lines => lines.forEach(l => $.ui.toast(l)))
    void loadProfiles($).then(p => update($, profiles, () => p))
    return next(e)
  })

  // Your words in chat ("review-agent=gpt-6.1-sol:medium as the reviewer") set your rules
  // directly; the model never relays them, so it cannot pass its own pick off as yours.
  on('prompt.submit', async ($, e, next) => {
    const flags = routesInText(e.text, await agentTypes($), await read($, catalogue))
    if (flags.length) {
      const p = parseArgs(flags.join(' '), await read($, routes))
      if (!p.errors.length) {
        await apply($, p.routes)
        $.ui.toast(`Route set by you: ${flags.join(' ').replace(/--agent |--/g, '')}`)
      }
    }
    return next(e)
  })

  // The pick log: one line per finished subagent, so the guide can learn from outcomes.
  on('turn.complete', async ($, e, next) => {
    const res = await next(e)
    const id = (e as { agentId?: string }).agentId
    const s = id ? (await read($, spawns))[id] : undefined
    if (s) {
      const home = (await $.env.get('HOME')) ?? ''
      const path = `${home}/.local/state/model-router/picks.jsonl`
      const line = JSON.stringify({ at: new Date().toISOString(), type: s.type, description: s.description, model: s.real ?? s.model, effort: s.effort, provider: s.provider, by: s.by, trail: s.trail, tokens: s.tokens, end: (e as { reason?: string }).reason })
      let prev = ''
      try { prev = await $.fs.read(path) } catch {}
      try { await $.fs.write(path, `${prev}${line}\n`) } catch {}
    }
    return res
  })

  on('prompt.compose', async ($, e, next) => {
    const res = await next(e)
    const guide = await guideText($)
    if (!guide) return res
    const r = await read($, routes)
    const cat = await read($, catalogue)
    const text = [
      'MODEL ROUTER. Subagent routes are yours to pick where the user set none; main is the user\'s only.',
      'Per spawn: end the Agent description with @MODEL:EFFORT (either part optional). For a whole agent type or a fallback chain, call the model-router route tool.',
      `Current routes:\n${describe(r, cat)}`,
      `Models by provider (live from submux):\n${cat.map(p => `${p.name}: ${p.ids.join(', ')}`).join('\n')}`,
      `Provider health: ${await healthText($)}`,
      'Pick log of past spawns and outcomes: ~/.local/state/model-router/picks.jsonl. At session end you may propose one guide line from it; the user approves.',
      guide,
    ].join('\n\n')
    return { ...res, sections: [...res.sections, { id: 'model-router', text, scope: 'session' }] }
  })

  on('tool.call', { tool: 'mcp__model-router__route' }, async ($, e) => {
    const args = String((e as unknown as { args?: unknown }).args ?? '')
    const out = sessionSet(await read($, routes), args)
    if (!out.routes) return { result: `refused: ${out.error}`, isError: true }
    await apply($, out.routes)
    $.ui.toast(`Route set by session: ${args}`)
    return { result: `ok. New spawns of these types use it.\n${describe(out.routes, await read($, catalogue))}` }
  })

  // A spawn's route is decided once, here, and frozen: later route changes reach new
  // spawns only, unless `/model ... --now`.
  on('agent.spawn', async ($, e, next) => {
    if (e.fork) return next(e)
    const tag = parseTag(e.description)
    const out = resolveSpawn({
      routes: await read($, routes), type: e.subagentType, tag, card: await cardOf($, e.subagentType),
      ...(e.model ? { param: e.model } : {}), parentModel: e.parentModel,
    })
    const retired = await retired$($)
    if (isRetired(out.model, retired)) return { deny: `model-router: ${out.model} is retired (routing guide). Use a current id; /model list shows them.` }
    const s: Spawn = {
      agentId: '', toolUseId: e.tool_use_id, type: e.subagentType, description: tag.description,
      model: out.model, ...(out.effort ? { effort: out.effort } : {}), ...(out.fallback ? { fallback: out.fallback } : {}),
      by: out.by, trail: [], tokens: 0, at: Date.now(),
    }
    // The agent's first step can run before next() resolves; turn.step claims it from here.
    await update($, pendingA, all => [...all, s])
    // The engine's own agent list labels the agent by this model, so hand it the routed id.
    const r = await next({ ...e, description: tag.description, model: out.model })
    await update($, pendingA, all => all.filter(x => x.toolUseId !== s.toolUseId))
    if (r.deny !== undefined || !r.agentId) return r
    await update($, spawns, all => (all[r.agentId!] ? all : { ...all, [r.agentId!]: { ...s, agentId: r.agentId! } }))
    return r
  })

  on('turn.step', async function* ($, e, next) {
    const now = await read($, routes)
    let s = e.agentId ? (await read($, spawns))[e.agentId] : undefined
    const pending = !s && e.agentId ? await read($, pendingA) : []
    if (pending.length) {
      const a = (await $.agent.list()).find(x => x.id === e.agentId)
      const p = a && pending.find(x => x.type === a.type && x.description === a.description)
      if (p) {
        await update($, pendingA, all => all.filter(x => x.toolUseId !== p.toolUseId))
        s = { ...p, agentId: e.agentId! }
        await update($, spawns, all => ({ ...all, [e.agentId!]: s! }))
      }
    }
    let model = e.model, effort = e.effort, fallback: string[] = []
    if (s) {
      model = s.model
      if (s.effort) effort = s.effort
      fallback = s.fallback ?? []
    } else if (!isEmpty(now)) {
      const type = e.agentId ? (await $.agent.list()).find(a => a.id === e.agentId)?.type : undefined
      const out = resolve(now, e, type, await $.session.model())
      model = out.model
      if (out.effort !== undefined) effort = out.effort
      fallback = out.fallback ?? []
    }
    const ids = [model, ...fallback]
    for (let k = 0; k < ids.length; k++) {
      const it = next({ ...e, model: ids[k]!, ...(effort === undefined ? {} : { effort }) })[Symbol.asyncIterator]()
      const held: TurnStepChunk[] = []
      let live = false
      while (true) {
        const c = await it.next()
        if (c.done) {
          const last = k === ids.length - 1
          if (!live && c.value.stopReason === null && !last && !next.signal?.aborted) break
          for (const h of held) yield h
          await record($, e.agentId, ids[k]!, effort, c.value, k > 0 ? ids.slice(0, k + 1) : [])
          return c.value
        }
        if (live) { yield c.value; continue }
        held.push(c.value)
        if (c.value.kind !== 'engine') { live = true; for (const h of held) yield h; held.length = 0 }
      }
    }
    throw new Error('model-router: fallback loop ended without a result')
  })

  // The built-in /model keeps main: bare opens its picker, `/model <id>` sets it. Everything
  // the router did (subagent routes, providers, profiles, fallbacks) is a /model subcommand.
  on('command.run', { command: 'model' }, async ($, e, next) => {
    const kind = modelArgs(e.args)
    if (kind === 'builtin') {
      // Refresh the picker's rows from submux just before the native picker opens.
      if (e.args.trim() === '') await tick($)
      return next(e)
    }
    if (kind === 'help' || /(^|\s)(--help|-h)(\s|$)/.test(e.args)) return { text: HELP }
    // Always the live catalogue (`submux models`), never the one cached at session start.
    const fresh = await loadCatalogue($)
    if (fresh.length) await update($, catalogue, () => fresh)
    const cat = fresh.length ? fresh : await read($, catalogue)
    if (kind === 'list') return { text: providersText(cat) }
    if (kind === 'table' || kind === 'short') {
      let r = await read($, routes)
      let note = ''
      if (kind === 'short') {
        const out = parseShort(e.args, r, cat)
        if (!out.routes) return { text: `model: ${out.error}` }
        await apply($, out.routes)
        r = out.routes
        note = 'Changed. New subagents use it from now on; running ones keep their model.\n\n'
      }
      const m = (await read($, seen)).main
      const main = m ? { model: m.model, ...(m.effort ? { effort: m.effort } : {}) } : { model: await $.session.model() }
      const recent = Object.values(await read($, spawns)).sort((x, y) => y.at - x.at).slice(0, 5)
      return { text: `${note}${routesTable(r, cat, main, recent)}${kind === 'table' ? `\n\nLive models\n${providersText(cat)}` : ''}` }
    }
    const cur = await read($, routes)
    const parsed = parseArgs(kind === 'agents' ? '--agents' : e.args, cur)
    if (parsed.errors.length) return { text: `model: ${parsed.errors.join('; ')}\n\n${HELP}` }
    const a = parsed.actions
    if (a.list) return { text: providersText(cat) }
    if (a.agents) {
      const live = new Map((await $.agent.list()).map(x => [x.id, x.status]))
      const m = (await read($, seen)).main
      const lines = [`main  ${m ? seenLine(m) : 'no reply yet'}`]
      for (const s of Object.values(await read($, spawns)).sort((x, y) => x.at - y.at)) {
        lines.push(`${s.type}(${s.description})  ${live.get(s.agentId) ?? 'gone'}  ${spawnLine(s, cat)}`)
      }
      return { text: lines.join('\n') }
    }
    let routesNext = parsed.routes
    const profs = await read($, profiles)
    const notes: string[] = []
    if (a.loadProfile) {
      const p = profs[a.loadProfile]
      if (!p) return { text: `model: no profile ${a.loadProfile}. Saved: ${Object.keys(profs).join(', ') || 'none'}` }
      routesNext = p
      notes.push(`Loaded profile ${a.loadProfile}.`)
    }
    await apply($, routesNext)
    if (a.now) {
      const running = new Set((await $.agent.list()).filter(x => x.status === 'running').map(x => x.id))
      const all = await read($, spawns)
      const moved: string[] = []
      for (const s of Object.values(all).filter(x => running.has(x.agentId))) {
        const tag = { ...(s.by.model === 'session' ? { model: s.model } : {}), ...(s.by.effort === 'session' && s.effort ? { effort: s.effort } : {}) }
        const out = resolveSpawn({ routes: routesNext, type: s.type, tag, card: await cardOf($, s.type), parentModel: await $.session.model() })
        if (out.model === s.model && out.effort === s.effort) continue
        all[s.agentId] = { ...s, model: out.model, ...(out.effort ? { effort: out.effort } : {}), by: out.by, trail: [...s.trail, `${s.model}/${s.effort ?? 'default'} → ${out.model}/${out.effort ?? 'default'} (you, now)`] }
        moved.push(`${s.type}(${s.description})`)
      }
      await update($, spawns, () => ({ ...all }))
      notes.push(moved.length ? `Switched now: ${moved.join(', ')}. Each re-reads its context once.` : 'No running subagent changed.')
    }
    if (a.saveProfile) {
      const all = { ...profs, [a.saveProfile]: routesNext }
      await $.store.set('profiles', all)
      await update($, profiles, () => all)
      notes.push(`Saved profile ${a.saveProfile}.`)
    }
    if (a.deleteProfile) {
      const all = { ...profs }
      delete all[a.deleteProfile]
      await $.store.set('profiles', all)
      await update($, profiles, () => all)
      notes.push(`Deleted profile ${a.deleteProfile}.`)
    }
    if (a.saveDefault) {
      await $.store.set('default', routesNext)
      notes.push('Saved as the default for new sessions.')
    }
    const unknown = unknownIds(routesNext, cat)
    if (unknown.length) notes.push(`Not in the submux catalogue (check spelling): ${unknown.join(', ')}`)
    const warn = contextWarning(routesNext, await $.session.model())
    if (warn) notes.push(warn)
    notes.push('Applies from the next request. A model or effort change re-reads the whole context once (no cache).')
    return { text: `${describe(routesNext, cat)}\n\n${notes.join('\n')}` }
  })

  // The spawn row: `coder(fix fee · gpt-6.1-sol/medium · ChatGPT Plus · by session · 41k tok)`.
  on('ui.render', { component: 'ToolUse', props: { tool: 'Agent' } }, async ($, e, next) => {
    const all = await read($, spawns)
    const s = Object.values(all).find(x => x.toolUseId === e.props.tool_use_id)
    const input = e.props.input as { description?: string } | undefined
    if (!s || !input) return next(e)
    const line = spawnLine(s, await read($, catalogue))
    return next({ ...e, props: { ...e.props, input: { ...input, description: `${s.description} · ${line}` } } })
  })

  // One compact tail on the hint line under the prompt, next to the engine's agent list:
  // main only when overridden, then each running subagent's real model. The engine's list
  // already shows names, time and tokens, so no band above the prompt repeats them.
  on('ui.render', { component: 'PromptHint' }, async ($, e, next) => {
    if (e.surface === 'mobile') return next(e)
    const cat = await read($, catalogue)
    const all = await read($, spawns)
    const running = new Set((await $.agent.list()).filter(a => a.status === 'running').map(a => a.id))
    const m = (await read($, seen)).main
    const r = await read($, routes)
    const parts: string[] = []
    if (m?.fellBackFrom || r.slots.main?.model || r.slots.main?.effort) {
      parts.push(`main ${m ? `${m.model}/${m.effort ?? 'default'}${m.fellBackFrom ? ` (fallback from ${m.fellBackFrom})` : ''}` : r.slots.main?.model ?? 'waiting'}`)
    }
    for (const s of Object.values(all).filter(x => running.has(x.agentId))) parts.push(`${s.type} ${tailLine(s, cat)}`)
    if (!parts.length) return next(e)
    return next({ ...e, props: { ...e.props, tail: `  ${parts.join(' · ')}` } })
  })

}

const HELP = `/model                          built-in picker for the main loop
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
Per spawn, the session ends an Agent description with @MODEL:EFFORT (e.g. "fix fee @gpt-6.1-sol:medium").
Value "off" clears one field (back to the launch default). Effort: ${EFFORTS.join(', ')}.`
