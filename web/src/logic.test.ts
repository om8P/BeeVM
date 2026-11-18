import { describe, expect, it } from 'vitest'
import { errorText, parseTiming, type InvokeEvent, type VM } from './api'
import type { Fn } from './api'
import { applyFunctionEvent, callsPerSecond, initialState, MAX_CALLS, pools, prunePulse, reduce } from './state'
import { fmtAxisMs, fmtBytes, fmtMs, percentile, summarize, yTicks, yTop } from './stats'

describe('latency chart y range', () => {
  it('Auto fits the slowest call, at least 2 s', () => {
    expect(yTop('auto', 0)).toBe(2_000)
    expect(yTop('auto', 90)).toBe(2_000)
    expect(yTop('auto', 1_700)).toBe(5_000) // 2 s would cut the dot: 20 % headroom
    expect(yTop('auto', 7_500)).toBe(10_000)
    expect(yTop('auto', 45_000)).toBe(60_000)
    expect(yTop('auto', 61_000)).toBe(120_000) // a 60 s timeout plus VM start
    expect(yTop('auto', 10 * 60_000)).toBe(120_000)
  })
  it('a fixed range ignores the data', () => {
    expect(yTop(2_000, 30_000)).toBe(2_000)
    expect(yTop(60_000, 3)).toBe(60_000)
  })
  it('ticks and labels fit the range', () => {
    expect(yTicks(2_000)).toEqual([1, 5, 10, 50, 100, 500, 1_000])
    expect(yTicks(60_000).slice(-3)).toEqual([10_000, 30_000, 60_000])
    expect([500, 1_000, 30_000, 60_000, 120_000].map(fmtAxisMs)).toEqual(['500', '1s', '30s', '1m', '2m'])
  })
})

const vm = (id: string, state: VM['state'], fn = 'hello', extra: Partial<VM> = {}): VM => ({
  id, function: fn, state, since: '2026-11-06T12:00:00Z', last_used: '2026-11-06T12:00:00Z', calls: 0, ...extra,
})

const call = (start: string, ms: number, at = '2026-11-06T12:00:00Z'): InvokeEvent => ({
  function: 'hello', runtime: 'binary', status: 200, start, boot_ms: 0, upload_ms: 0, load_ms: 0,
  function_ms: 1, duration_ms: ms, at,
})

describe('parseTiming', () => {
  it('reads the X-BeeVM headers', () => {
    const h = new Headers({
      'X-BeeVM-VM-Id': 'vm-7', 'X-BeeVM-Start': 'restore', 'X-BeeVM-Snapshot': 'function',
      'X-BeeVM-Boot-Ms': '45', 'X-BeeVM-Upload-Ms': '0', 'X-BeeVM-Load-Ms': '0',
      'X-BeeVM-Function-Ms': '3', 'X-BeeVM-Total-Ms': '52', 'X-BeeVM-Code-Cache': 'hit', 'X-BeeVM-Fetch-Ms': '0',
    })
    expect(parseTiming(h)).toEqual({
      vm: 'vm-7', start: 'restore', snapshot: 'function', codeCache: 'hit',
      bootMs: 45, uploadMs: 0, loadMs: 0, functionMs: 3, totalMs: 52, fetchMs: 0,
    })
  })
  it('gives zeros when headers are missing', () => {
    const t = parseTiming(new Headers())
    expect(t.start).toBe('')
    expect(t.bootMs).toBe(0)
  })
  it('extracts error messages', () => {
    expect(errorText(503, '{"error":"no free VM slot"}')).toBe('no free VM slot')
    expect(errorText(500, 'boom\n')).toBe('boom')
    expect(errorText(404, '')).toBe('HTTP 404')
  })
})

describe('reduce', () => {
  it('replaces a pending VM by its real ID and removes destroyed ones', () => {
    let s = reduce(initialState, { type: 'hello', hello: { vms: [vm('vm-1', 'idle')], max_vms: 6 } })
    expect(s.maxVMs).toBe(6)
    s = reduce(s, { type: 'vm', vm: vm('pending-2', 'restoring'), now: 0 })
    expect(Object.keys(s.vms).sort()).toEqual(['pending-2', 'vm-1'])
    s = reduce(s, { type: 'vm', vm: vm('vm-2', 'busy', 'hello', { prev_id: 'pending-2' }), now: 1000 })
    expect(Object.keys(s.vms).sort()).toEqual(['vm-1', 'vm-2'])
    expect(s.vms['vm-2'].prev_id).toBeUndefined()
    expect(s.pulse['vm-2']).toBe(1400)
    s = reduce(s, { type: 'vm', vm: vm('vm-1', 'destroyed'), now: 0 })
    expect(Object.keys(s.vms)).toEqual(['vm-2'])
    expect(prunePulse(s.pulse, 2000)).toEqual({})
  })

  it('hello keeps the call history', () => {
    let s = reduce(initialState, { type: 'invoke', call: call('warm', 1), now: 0 })
    s = reduce(s, { type: 'hello', hello: { vms: [], max_vms: 2 } })
    expect(s.calls).toHaveLength(1)
  })

  it('keeps at most MAX_CALLS calls', () => {
    let s = initialState
    for (let i = 0; i < MAX_CALLS + 5; i++) s = reduce(s, { type: 'invoke', call: call('warm', i), now: 0 })
    expect(s.calls).toHaveLength(MAX_CALLS)
    expect(s.calls[0].duration_ms).toBe(5)
    expect(s.calls[MAX_CALLS - 1].seq).toBe(MAX_CALLS + 5)
  })
})

describe('applyFunctionEvent', () => {
  const fn = (name: string, sha = 'a'): Fn => ({
    name, runtime: 'binary', code_sha256: sha, code_size: 1, memory_mib: 128, vcpus: 1, timeout_ms: 10000,
    created_at: '', updated_at: '',
  })
  it('adds in name order, replaces on redeploy, removes on delete', () => {
    let list = applyFunctionEvent([fn('b')], { action: 'deployed', function: fn('a') })
    expect(list.map((f) => f.name)).toEqual(['a', 'b'])
    list = applyFunctionEvent(list, { action: 'deployed', function: fn('a', 'new') })
    expect(list).toHaveLength(2)
    expect(list[0].code_sha256).toBe('new')
    list = applyFunctionEvent(list, { action: 'deleted', function: { ...fn('b'), runtime: '' } })
    expect(list.map((f) => f.name)).toEqual(['a'])
    expect(applyFunctionEvent(list, { action: 'deleted', function: fn('zzz') })).toEqual(list)
  })
})

describe('pools and rates', () => {
  it('counts VMs per function and state', () => {
    const p = pools([vm('a', 'idle'), vm('b', 'busy'), vm('c', 'idle', 'py'), vm('d', 'restoring', '')])
    expect(p.map((x) => [x.name, x.idle, x.busy, x.restoring, x.total])).toEqual([
      ['(removed)', 0, 0, 1, 1],
      ['hello', 1, 1, 0, 2],
      ['py', 1, 0, 0, 1],
    ])
  })
  it('counts calls per second in a window', () => {
    let s = initialState
    for (const at of ['2026-11-06T12:00:00Z', '2026-11-06T12:00:08Z', '2026-11-06T12:00:09Z']) {
      s = reduce(s, { type: 'invoke', call: call('warm', 1, at), now: 0 })
    }
    expect(callsPerSecond(s.calls, Date.parse('2026-11-06T12:00:10Z'), 5000)).toBe(0.4)
  })
})

describe('stats', () => {
  it('percentile by nearest rank', () => {
    const v = Array.from({ length: 100 }, (_, i) => i + 1)
    expect(percentile(v, 50)).toBe(50)
    expect(percentile(v, 99)).toBe(99)
    expect(percentile([7], 99)).toBe(7)
    expect(percentile([], 50)).toBeNaN()
  })
  it('summarizes per start kind in a fixed order', () => {
    const calls = [call('restore', 80), call('warm', 1), call('warm', 3), call('boot', 900)]
    const s = summarize(calls, (c) => c.start ?? '', (c) => c.duration_ms)
    expect(s.map((x) => [x.kind, x.count, x.p50])).toEqual([
      ['warm', 2, 1], ['restore', 1, 80], ['boot', 1, 900],
    ])
  })
  it('formats', () => {
    expect(fmtMs(1.234)).toBe('1.2 ms')
    expect(fmtMs(45.6)).toBe('46 ms')
    expect(fmtMs(12_345)).toBe('12.3 s')
    expect(fmtBytes(6.2 * (1 << 20))).toBe('6.2 MiB')
    expect(fmtBytes(0)).toBe('–')
  })
})
