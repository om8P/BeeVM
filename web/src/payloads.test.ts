import { expect, it } from 'vitest'
import { loadPayload, savePayload } from './payloads'

it('remembers the payload per function, else a sample', () => {
  const m = new Map<string, string>()
  const store = { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) } as Storage
  expect(loadPayload('hello', store)).toBe('{"name":"BeeVM"}')
  savePayload('hello', '{"x":1}', store)
  expect(loadPayload('hello', store)).toBe('{"x":1}')
  expect(loadPayload('other', store)).toBe('{}')
})
