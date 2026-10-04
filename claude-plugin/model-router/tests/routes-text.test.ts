import { test, expect } from 'claude-code/testing'
import { EMPTY, modelArgs, parseCatalogue, parseShort, providersText, routesTable, shortProvider, tailLine } from '../hooks/route'
import type { Spawn } from '../types'

const cat = parseCatalogue('Claude Max (this CLI\'s own login) (1 ids): claude-opus-5-5\nChatGPT (Codex) (1 ids): gpt-6.1-sol\n')

test('short provider drops the parenthetical', () => {
  expect(shortProvider('Kiro (kirocc :3456)')).toBe('Kiro')
})

test('tail line: real model, short effort, short provider', () => {
  const s = { agentId: 'a', toolUseId: 't', type: 'legwork', description: 'd', model: 'gpt-6.1-sol', effort: 'medium', by: { model: 'session', effort: 'session' }, trail: [], tokens: 0, at: 0 } as Spawn
  expect(tailLine(s, cat)).toBe('gpt-6.1-sol/med (ChatGPT)')
})

test('modelArgs: bare and one id stay built-in, the rest is the router', () => {
  expect(modelArgs('')).toBe('builtin')
  expect(modelArgs('opus')).toBe('builtin')
  expect(modelArgs('kiro/claude-opus-5.5')).toBe('builtin')
  expect(modelArgs('routes')).toBe('table')
  expect(modelArgs('list')).toBe('list')
  expect(modelArgs('agents')).toBe('agents')
  expect(modelArgs('help')).toBe('help')
  expect(modelArgs('sonnet gpt-6.1-sol high')).toBe('short')
  expect(modelArgs('--agent coder=:high --now')).toBe('flags')
})

test('parseShort: tier, effort only, agent, off, main refused, unknown id', () => {
  const r1 = parseShort('sonnet gpt-6.1-sol high', EMPTY, cat).routes!
  expect(r1.slots.sonnet).toEqual({ model: 'gpt-6.1-sol', effort: 'high' })
  expect(parseShort('nomodel low', EMPTY, cat).routes!.slots.inherit).toEqual({ effort: 'low' })
  const r2 = parseShort('coder gpt-6.1-sol', r1, cat).routes!
  expect(r2.agents.coder).toEqual({ model: 'gpt-6.1-sol' })
  expect(parseShort('sonnet off', r2, cat).routes!.slots.sonnet).toBeUndefined()
  expect(parseShort('main gpt-6.1-sol', EMPTY, cat).error).toContain('/model')
  expect(parseShort('sonnet gpt-9', EMPTY, cat).error).toContain('not a model id')
})

test('table: no fixed model claimed for subagents; rules and real spawns listed', () => {
  const t0 = routesTable(EMPTY, cat, { model: 'claude-opus-5-5', effort: 'medium' }, [])
  expect(t0).toContain('Main loop: claude-opus-5-5 · medium · Claude Max')
  expect(t0).toContain('Subagents: picked per spawn')
  expect(t0).not.toContain('same as main')
  expect(t0).toContain('none yet this session')
  const r = parseShort('sonnet gpt-6.1-sol high', EMPTY, cat).routes!
  const spawn = { agentId: 'a', toolUseId: 't', type: 'coder', description: 'fix fee', model: 'gpt-6.1-sol', effort: 'medium' as const,
    by: { model: 'session' as const, effort: 'session' as const }, trail: [], tokens: 0, at: 1 }
  const t = routesTable(r, cat, { model: 'claude-opus-5-5' }, [spawn])
  expect(t).toMatch(/Sonnet agents +gpt-6.1-sol · high · ChatGPT/)
  expect(t).toMatch(/coder \(fix fee\) +gpt-6.1-sol · medium · ChatGPT · picked by session/)
  expect(providersText(cat)).toContain('ChatGPT      gpt-6.1-sol')
})
