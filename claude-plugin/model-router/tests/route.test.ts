import { test, expect } from 'claude-code/testing'
import { contextWarning, fromSubmuxProfile, staleLogin, parseArgs, parseCatalogue, resolve, statusText, unknownIds } from '../hooks/route'
import { EMPTY, isRetired, parseCard, parseTag, resolveSpawn, retiredIds, routesInText, sessionSet, spawnLine } from '../hooks/route'
import type { SpawnIn } from '../hooks/route'
import type { Routes } from '../types'

const S = 'claude-sonnet-5-5'
const cat = parseCatalogue('Kiro (kirocc :3456) (2 ids): kiro/claude-opus-5.5, kiro/claude-sonnet-5.5\nChatGPT (Codex) (1 ids): gpt-6.1-sol\nnoise line\n')

test('catalogue: parses providers and ids', () => {
  expect(cat).toEqual([
    { name: 'Kiro (kirocc :3456)', ids: ['kiro/claude-opus-5.5', 'kiro/claude-sonnet-5.5'] },
    { name: 'ChatGPT (Codex)', ids: ['gpt-6.1-sol'] },
  ])
})

test('resolve: empty routes pass the step through', () => {
  expect(resolve(EMPTY, { model: S, effort: 'high' }, undefined, S)).toEqual({ model: S, effort: 'high' })
})

test('resolve: main rule rewrites only the main loop', () => {
  const r = { slots: { main: { model: 'gpt-6.1-sol', effort: 'low' as const } }, agents: {} }
  expect(resolve(r, { model: S, effort: 'high' }, undefined, S)).toEqual({ model: 'gpt-6.1-sol', effort: 'low' })
  // a haiku-tier subagent is not main
  expect(resolve(r, { model: 'claude-haiku-4-5-20251001', agentId: 'a1' }, 'general-purpose', S)).toEqual({ model: 'claude-haiku-4-5-20251001', agentId: 'a1' })
})

test('resolve: unrecorded subagent on the session model never takes main rules', () => {
  const r = { slots: { main: { model: 'gpt-6.1-sol', effort: 'low' as const } }, agents: {} }
  expect(resolve(r, { model: 'x-model', effort: 'xhigh', agentId: 'a1' }, 'review-agent-critical', 'x-model')).toEqual({ model: 'x-model', effort: 'xhigh', agentId: 'a1' })
})

test('resolve: tier rule by family, agent type beats tier per field', () => {
  const r = { slots: { haiku: { model: 'glm-5.3', effort: 'low' as const } }, agents: { Explore: { model: 'kiro/claude-opus-5.5' } } }
  expect(resolve(r, { model: 'claude-haiku-4-5-20251001', agentId: 'a1' }, 'grunt', S)).toEqual({ model: 'glm-5.3', effort: 'low', agentId: 'a1' })
  expect(resolve(r, { model: 'claude-haiku-4-5-20251001', agentId: 'a2' }, 'Explore', S)).toEqual({ model: 'kiro/claude-opus-5.5', effort: 'low', agentId: 'a2' })
})

test('args: claude-style flags build routes', () => {
  const p = parseArgs('--model kiro/claude-opus-5.5 --effort medium --sonnet glm-5.3 --sonnet-effort low --agent Explore=gpt-6.1-sol:high', EMPTY)
  expect(p.errors).toEqual([])
  expect(p.routes).toEqual({
    slots: { main: { model: 'kiro/claude-opus-5.5', effort: 'medium' }, sonnet: { model: 'glm-5.3', effort: 'low' } },
    agents: { Explore: { model: 'gpt-6.1-sol', effort: 'high' } },
  })
})

test('args: off clears a field, empty agent model allowed, bad effort rejected', () => {
  const start = { slots: { main: { model: 'x', effort: 'low' as const } }, agents: { coder: { model: 'y' } } }
  const p = parseArgs('--effort off --agent coder=:high', start)
  expect(p.routes).toEqual({ slots: { main: { model: 'x' } }, agents: { coder: { model: 'y', effort: 'high' } } })
  expect(parseArgs('--effort turbo', EMPTY).errors.length).toBe(1)
  expect(parseArgs('--model', EMPTY).errors).toEqual(['--model needs a value'])
})

test('args: bare word loads a profile, no args shows the table', () => {
  expect(parseArgs('kiro/claude', EMPTY).actions.loadProfile).toBe('kiro/claude')
  expect(parseArgs('', EMPTY).actions.show).toBe(true)
})

test('submux profile import', () => {
  expect(fromSubmuxProfile({ main: 'kiro/claude-opus-5.5', sonnet: 'kiro/claude-sonnet-5.5', effort: 'medium' })).toEqual({
    slots: { main: { model: 'kiro/claude-opus-5.5', effort: 'medium' }, sonnet: { model: 'kiro/claude-sonnet-5.5' } },
    agents: {},
  })
})

test('status, unknown ids, context warning', () => {
  const r = { slots: { main: { model: 'gpt-6.1-sol', effort: 'low' as const } }, agents: { Explore: { model: 'kiro/claude-sonnet-5-5' } } }
  expect(statusText(EMPTY)).toBe(undefined)
  expect(statusText(r)).toBe('⇄ main:gpt-6.1-sol/low · @Explore:kiro/claude-sonnet-5-5')
  expect(unknownIds(r, cat)).toEqual(['kiro/claude-sonnet-5-5'])
  expect(contextWarning(r, S)?.startsWith('Context is still counted as claude-sonnet-5-5')).toBe(true)
  expect(contextWarning({ slots: { main: { model: 'claude-opus-5-5' } }, agents: {} }, S)).toBe(undefined)
})

const spawn = (routes: Routes, over: Partial<SpawnIn> = {}) =>
  resolveSpawn({ routes, type: 'review-agent-critical', tag: {}, card: { model: 'sonnet', effort: 'xhigh' }, parentModel: S, ...over })

test('spawn: main effort never leaks into a card effort (the reported bug)', () => {
  const r: Routes = { slots: { main: { model: 'kiro/claude-opus-5.5', effort: 'low' } }, agents: {} }
  expect(spawn(r)).toEqual({ model: 'sonnet', effort: 'xhigh', by: { model: 'engine', effort: 'card' } })
})

test('spawn: tier map remaps a card model, card effort kept', () => {
  const r: Routes = { slots: { sonnet: { model: 'kiro/claude-sonnet-5.5', effort: 'low' } }, agents: {} }
  expect(spawn(r)).toEqual({ model: 'kiro/claude-sonnet-5.5', effort: 'xhigh', by: { model: 'tier', effort: 'card' } })
})

test('spawn: your rule beats the session tag, tag beats the card', () => {
  const r: Routes = { slots: {}, agents: { 'review-agent-critical': { model: 'gpt-6.1-sol' } } }
  expect(spawn(r, { tag: { model: 'glm-5.3', effort: 'medium' } })).toEqual({ model: 'gpt-6.1-sol', effort: 'medium', by: { model: 'you', effort: 'session' } })
  const soft: Routes = { slots: {}, agents: { 'review-agent-critical': { model: 'gpt-6.1-sol', by: 'session' } } }
  expect(spawn(soft, { tag: { model: 'glm-5.3' } }).model).toBe('glm-5.3')
})

test('spawn: a true inheritor follows inherit, then main', () => {
  const r: Routes = { slots: { main: { model: 'gpt-6.1-sol', effort: 'high' } }, agents: {} }
  expect(spawn(r, { type: 'general-purpose', card: {} })).toEqual({ model: 'gpt-6.1-sol', effort: 'high', by: { model: 'main', effort: 'main' } })
})

test('spawn: fallback list comes from the rule', () => {
  const r: Routes = { slots: {}, agents: { coder: { model: 'gpt-6.1-sol', fallback: ['gpt-6.1-sol', 'kiro/claude-opus-5.5'] } } }
  expect(spawn(r, { type: 'coder', card: {} }).fallback).toEqual(['kiro/claude-opus-5.5'])
})

test('tag, card, retired, fallback flag', () => {
  expect(parseTag('fix fee @gpt-6.1-sol:medium')).toEqual({ description: 'fix fee', model: 'gpt-6.1-sol', effort: 'medium' })
  expect(parseTag('fix fee @:high')).toEqual({ description: 'fix fee', effort: 'high' })
  expect(parseTag('mail a@b.com')).toEqual({ description: 'mail a@b.com' })
  expect(parseCard('---\nname: x\nmodel: sonnet\neffort: xhigh\n---\nbody')).toEqual({ model: 'sonnet', effort: 'xhigh' })
  expect(parseCard('---\nmodel: inherit\n---')).toEqual({})
  const ret = retiredIds('- hurry => fast\n- retired: claude-opus-5, gpt-6-sol\n')
  expect(ret).toEqual(['claude-opus-5', 'gpt-6-sol'])
  expect([isRetired('kiro/claude-opus-5[1m]', ret), isRetired('claude-opus-5-5', ret), isRetired('gpt-6-sol', ret)]).toEqual([true, false, true])
  const p = parseArgs('--fallback coder=kiro/claude-opus-5.5,glm-5.3', EMPTY)
  expect(p.routes.agents.coder).toEqual({ fallback: ['kiro/claude-opus-5.5', 'glm-5.3'] })
  expect(parseArgs('--fallback coder=off', p.routes).routes.agents).toEqual({})
})

test('spawn line: real model, provider quota, who, trail, tokens', () => {
  const s = { agentId: 'a', toolUseId: 't', type: 'coder', description: 'fix', model: 'gpt-6.1-sol', effort: 'medium' as const,
    by: { model: 'session' as const, effort: 'session' as const }, trail: [], tokens: 41200, at: 0 }
  expect(spawnLine(s, cat, { 'ChatGPT (Codex)': 68 })).toBe('gpt-6.1-sol/medium · ChatGPT (Codex) 68% · by session · 41k tok')
  expect(spawnLine({ ...s, by: { model: 'you', effort: 'card' }, tokens: 0, trail: ['gpt-6.1-sol', 'kiro/claude-opus-5.5'], real: 'kiro/claude-opus-5.5' }, cat))
    .toBe('kiro/claude-opus-5.5/medium · Kiro (kirocc :3456) · by you/card · trail gpt-6.1-sol → kiro/claude-opus-5.5')
})

test('routes from your words: only slots, known agent types and catalogue ids', () => {
  const text = 'use review-agent=gpt-6.1-sol:medium as the reviewer, coder=:high, sonnet=kiro/claude-sonnet-5.5; x=1 and foo=gpt-6.1-sol'
  expect(routesInText(text, ['review-agent', 'coder'], cat)).toEqual(['--agent review-agent=gpt-6.1-sol:medium', '--agent coder=:high', '--sonnet kiro/claude-sonnet-5.5'])
})

test('session set: subagents only, never over your rule', () => {
  const cur: Routes = { slots: {}, agents: { coder: { model: 'gpt-6.1-sol' }, Explore: { model: 'glm-5.3', by: 'session' } } }
  expect(sessionSet(cur, '--model x').error).toContain("the user's")
  expect(sessionSet(cur, '--agent coder=glm-5.3').error).toContain('set by the user')
  expect(sessionSet(cur, '--agent Explore=:low --fallback grunt=glm-5.3').routes?.agents).toEqual({
    coder: { model: 'gpt-6.1-sol' }, Explore: { model: 'glm-5.3', effort: 'low', by: 'session' }, grunt: { fallback: ['glm-5.3'], by: 'session' },
  })
  // you touching a session rule makes it yours
  expect(parseArgs('--agent Explore=:high', cur).routes.agents.Explore).toEqual({ model: 'glm-5.3', effort: 'high' })
})

test('stale login: only enabled logins expired over a day', () => {
  const now = Date.parse('2026-10-03T12:00:00+05:30')
  expect(staleLogin({ type: 'codex', email: 'a@b', expired: '2026-10-01T10:00:00+05:30' }, now))
    .toBe('codex login (a@b) stopped refreshing, expired 2026-10-01T10:00:00+05:30; re-login: cliproxyapi -codex-login')
  expect(staleLogin({ type: 'codex', expired: '2026-10-03T00:00:00+05:30' }, now)).toBe(undefined)
  expect(staleLogin({ type: 'codex', expired: '2026-09-01T00:00:00Z', disabled: true }, now)).toBe(undefined)
  expect(staleLogin({ type: 'codex' }, now)).toBe(undefined)
})
