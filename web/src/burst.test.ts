import { expect, it } from 'vitest'
import type { InvokeEvent } from './api'
import { collectAsync } from './burst'

const ev = (request_id?: string): InvokeEvent => ({
  function: 'f',
  runtime: 'python',
  status: 200,
  vm: 'vm1',
  boot_ms: 0,
  upload_ms: 0,
  load_ms: 0,
  function_ms: 3,
  duration_ms: 12,
  at: '2026-11-19T10:00:00.000Z',
  request_id,
})

it('collects each wanted async result once', () => {
  const { found } = collectAsync([ev('a'), ev(), ev('other'), ev('a'), ev('b')], ['a', 'b', 'c'])
  expect(found.length).toBe(2)
})
