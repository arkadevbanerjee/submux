import { test, expect } from 'claude-code/testing'
import { KIROCC, kiroRewrite, kiroTarget, isThreaded } from '../hooks/route'

const A = 'https://api.anthropic.com/v1/messages?beta=true'
const H = { authorization: '[redacted]', 'x-api-key': '[redacted]', 'anthropic-beta': 'x', 'X-Claude-Code-Session-Id': '[redacted]', 'x-other-secret': '[redacted]' }
const body = (model: string) => JSON.stringify({ model, max_tokens: 5, messages: [] })

test('kiro model: sent to kirocc with bare id, bearer key, Anthropic creds dropped', () => {
  const r = kiroRewrite(A, H, body('kiro/claude-haiku-5.5'), 'K', 'sess')!
  expect(r.url).toBe(`${KIROCC}/v1/messages?beta=true`)
  expect(JSON.parse(r.body).model).toBe('claude-haiku-5.5')
  expect(r.headers.authorization).toBe('Bearer K')
  expect(r.headers['x-api-key']).toBeUndefined()
  expect(r.headers['anthropic-beta']).toBe('x')
  expect(r.headers['x-claude-code-session-id']).toBe('sess')
  expect(r.headers['X-Claude-Code-Session-Id']).toBeUndefined()
  expect(r.headers['x-other-secret']).toBeUndefined()
})

test('count_tokens path keeps its path', () => {
  expect(kiroRewrite('https://api.anthropic.com/v1/messages/count_tokens', {}, body('kiro/claude-opus-5.5'), 'K', 'sess')!.url).toBe(`${KIROCC}/v1/messages/count_tokens`)
})

test('session id: written from the session, fallback when empty', () => {
  expect(kiroRewrite(A, {}, body('kiro/x'), 'K', 'sess')!.headers['x-claude-code-session-id']).toBe('sess')
  expect(kiroRewrite(A, {}, body('kiro/x'), 'K', '')!.headers['x-claude-code-session-id']).toBe('model-router')
})

test('left alone: claude model, no key, non-JSON, base64 body, other path, bare prefix', () => {
  expect(kiroRewrite(A, H, body('claude-opus-5-5'), 'K', 'sess')).toBeUndefined()
  expect(kiroRewrite(A, H, body('kiro/claude-haiku-5.5'), '', 'sess')).toBeUndefined()
  expect(kiroRewrite(A, H, 'not json', 'K', 'sess')).toBeUndefined()
  expect(kiroRewrite(A, H, { base64: 'AA' }, 'K', 'sess')).toBeUndefined()
  expect(kiroRewrite('https://api.anthropic.com/api/oauth/usage', H, body('kiro/x'), 'K', 'sess')).toBeUndefined()
  expect(kiroRewrite(A, H, body('kiro/'), 'K', 'sess')).toBeUndefined()
  expect(kiroRewrite('not a url', H, body('kiro/x'), 'K', 'sess')).toBeUndefined()
})

test('kiroTarget: submux id mapping, exact ids kept', () => {
  expect(kiroTarget(A, body('kiro/claude-opus-5-5'))).toBe('claude-opus-5.5')
  expect(kiroTarget(A, body('kiro/claude-opus-5-5[1m]'))).toBe('claude-opus-5.5')
  expect(kiroTarget(A, body('kiro/claude-haiku-5.5'))).toBe('claude-haiku-5.5')
  expect(kiroTarget(A, body('kiro/gpt-5.6-terra'))).toBe('gpt-5.6-terra')
  expect(kiroTarget(A, body('claude-opus-5-5'))).toBeUndefined()
})

test('main-session body that merely mentions kiro/ is not a kiro request', () => {
  const b = JSON.stringify({ model: 'claude-opus-5-5', messages: [{ role: 'user', content: 'route to "kiro/claude-haiku-5.5"' }] })
  expect(kiroTarget(A, b)).toBeUndefined()
})

test('isThreaded: create and continue yes, stateless and junk no', () => {
  expect(isThreaded(JSON.stringify({ model: 'kiro/x', thread: { type: 'continue', previous_message_id: 'm' } }))).toBe(true)
  expect(isThreaded(JSON.stringify({ model: 'kiro/x', thread: { type: 'create' } }))).toBe(true)
  expect(isThreaded(body('kiro/x'))).toBe(false)
  expect(isThreaded('not json')).toBe(false)
  expect(isThreaded({ base64: 'AA' })).toBe(false)
})

test('host gate: a session behind a local router is left alone', () => {
  expect(kiroTarget('http://127.0.0.1:8787/v1/messages', body('kiro/claude-haiku-5.5'))).toBeUndefined()
  expect(kiroRewrite('http://127.0.0.1:8787/v1/messages', {}, body('kiro/claude-haiku-5.5'), 'K', 's')).toBeUndefined()
})
