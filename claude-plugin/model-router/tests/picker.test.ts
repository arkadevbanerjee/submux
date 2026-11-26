import { test, expect } from 'claude-code/testing'
import { pickerIds } from '../hooks/route'

const retired = ['claude-opus-5', 'claude-sonnet-5', 'gpt-6-sol', 'gpt-5.6-luna', 'claude-gpt-5.6-luna']

test('picker drops native Claude ids, retired ids (any prefix or [1m]), Sonnet and old generations', () => {
  const ids = [
    'claude-opus-5-5', 'kiro/auto', 'kiro/claude-gpt-5.6-sol', 'kiro/claude-gpt-5.6-luna',
    'kiro/claude-haiku-4.5', 'kiro/claude-haiku-5.5', 'kiro/claude-opus-4-8[1m]', 'kiro/claude-opus-4.7',
    'kiro/claude-opus-5[1m]', 'kiro/claude-opus-5', 'kiro/claude-opus-5.5', 'kiro/claude-opus-5-5[1m]',
    'kiro/claude-sonnet-5.5', 'ag/claude-sonnet-5-5-high', 'ag/claude-opus-4-6-thinking', 'ag/claude-opus-5-5-high',
    'kiro/gpt-5.6-luna', 'gpt-6-luna', 'grok-4.7',
  ]
  expect(pickerIds(ids, retired)).toEqual([
    'kiro/auto', 'kiro/claude-gpt-5.6-sol', 'kiro/claude-haiku-5.5',
    'kiro/claude-opus-5.5', 'kiro/claude-opus-5-5[1m]', 'ag/claude-opus-5-5-high', 'gpt-6-luna', 'grok-4.7',
  ])
})
