// Bursts in the Invoke panel: many calls of one function at once, summed up afterwards.
//
// Synchronous calls carry their timing in the answer. Async calls (POST /invoke/{name}?async=1)
// are answered at once with a request id; their result arrives later as an "invoke" event on
// the live stream with the same request_id. Both become a BurstCall.

import type { InvokeEvent, InvokeResult } from './api'

export interface BurstCall {
  vm?: string
  start?: string // warm, restore, boot
  ms?: number // sync: round trip in the browser; async: time in the server, queue included
  queuedMs?: number // async: time waiting in the server's queue
  status?: number
  error?: string // the call did not run, or failed (async: status and reason)
}

export interface Burst {
  calls: BurstCall[]
  wallMs: number // from the first request sent to the last result
  async: boolean
}

export function fromResult(r: InvokeResult): BurstCall {
  return { vm: r.timing.vm, start: r.timing.start, ms: r.roundTripMs, status: r.status }
}

export function fromEvent(ev: InvokeEvent): BurstCall {
  const c: BurstCall = { vm: ev.vm, start: ev.start, ms: ev.duration_ms, queuedMs: ev.queued_ms ?? 0, status: ev.status }
  if (ev.error) c.error = `${ev.status}: ${ev.error}`
  return c
}

// collectAsync finds the results of the async calls `ids` among the events received so far.
// Events may arrive before the 202 answer that told us the id, so this looks at all of them.
// lastAt is when the latest of the found calls finished (ms since 1970), 0 if none.
export function collectAsync(events: InvokeEvent[], ids: string[]): { found: BurstCall[]; lastAt: number } {
  const want = new Set(ids)
  const found: BurstCall[] = []
  let lastAt = 0
  for (const ev of events) {
    if (!ev.request_id || !want.has(ev.request_id)) continue
    want.delete(ev.request_id) // an id counts once
    found.push(fromEvent(ev))
    lastAt = Math.max(lastAt, Date.parse(ev.at) || 0)
  }
  return { found, lastAt }
}
