// The dashboard's live state, changed only by events from the server (a reducer: a pure
// function from (old state, event) to new state, which makes it easy to test).

import type { Hello, InvokeEvent, VM } from './api'

export const MAX_CALLS = 600 // how many recent calls the chart keeps

export interface Call extends InvokeEvent {
  seq: number // increasing number, a stable key for React
  t: number // when it finished, ms since 1970
}

export interface LiveState {
  vms: Record<string, VM>
  maxVMs: number
  calls: Call[]
  pulse: Record<string, number> // VM id -> time until which its tile flashes
  seq: number
}

export const initialState: LiveState = { vms: {}, maxVMs: 0, calls: [], pulse: {}, seq: 0 }

export type Action =
  | { type: 'hello'; hello: Hello }
  | { type: 'vm'; vm: VM; now: number }
  | { type: 'invoke'; call: InvokeEvent; now: number }
  | { type: 'reset' }

export function reduce(s: LiveState, a: Action): LiveState {
  switch (a.type) {
    case 'reset':
      return initialState
    case 'hello': {
      // The full picture: replace the VMs, keep the call history.
      const vms: Record<string, VM> = {}
      for (const v of a.hello.vms) vms[v.id] = v
      return { ...s, vms, maxVMs: a.hello.max_vms }
    }
    case 'vm': {
      const vms = { ...s.vms }
      const v = a.vm
      if (v.prev_id) delete vms[v.prev_id] // "pending-N" got its real ID
      if (v.state === 'destroyed') {
        delete vms[v.id]
      } else {
        const { prev_id: _ignored, ...rest } = v
        void _ignored
        vms[v.id] = rest
      }
      let pulse = s.pulse
      if (v.state === 'busy') pulse = { ...pulse, [v.id]: a.now + 400 }
      return { ...s, vms, pulse }
    }
    case 'invoke': {
      const seq = s.seq + 1
      const call: Call = { ...a.call, seq, t: Date.parse(a.call.at) || a.now }
      const calls = s.calls.length >= MAX_CALLS ? [...s.calls.slice(1 - MAX_CALLS), call] : [...s.calls, call]
      return { ...s, calls, seq }
    }
  }
}

// prunePulse forgets finished flashes (called now and then, so the map does not grow).
export function prunePulse(pulse: Record<string, number>, now: number): Record<string, number> {
  const out: Record<string, number> = {}
  for (const [id, until] of Object.entries(pulse)) if (until > now) out[id] = until
  return out
}

export interface FunctionPool {
  name: string
  restoring: number
  idle: number
  busy: number
  recycling: number
  total: number
}

// pools counts each function's VMs by state.
export function pools(vms: VM[]): FunctionPool[] {
  const by = new Map<string, FunctionPool>()
  for (const v of vms) {
    const name = v.function || '(removed)'
    let p = by.get(name)
    if (!p) {
      p = { name, restoring: 0, idle: 0, busy: 0, recycling: 0, total: 0 }
      by.set(name, p)
    }
    if (v.state !== 'destroyed') p[v.state]++
    p.total++
  }
  return [...by.values()].sort((a, b) => a.name.localeCompare(b.name))
}

// callsPerSecond counts calls that finished in the last `window` ms.
export function callsPerSecond(calls: Call[], now: number, window = 5000): number {
  let n = 0
  for (let i = calls.length - 1; i >= 0 && calls[i].t >= now - window; i--) n++
  return n / (window / 1000)
}
