// Small number helpers for the dashboard.

// percentile returns the p-th percentile (0..100) of values by the nearest-rank method (the
// smallest value with at least p % of the values at or below it), or NaN for no values.
export function percentile(values: number[], p: number): number {
  if (values.length === 0) return NaN
  const sorted = [...values].sort((a, b) => a - b)
  const rank = Math.ceil((p / 100) * sorted.length)
  return sorted[Math.min(Math.max(rank, 1), sorted.length) - 1]
}

export interface KindSummary {
  kind: string
  count: number
  p50: number
  p99: number
}

// summarize groups calls by how their VM started and returns p50/p99 per group, in a fixed
// order (warm, restore, boot, then anything else).
export function summarize<T>(calls: T[], kind: (c: T) => string, ms: (c: T) => number): KindSummary[] {
  const groups = new Map<string, number[]>()
  for (const c of calls) {
    const k = kind(c)
    if (!groups.has(k)) groups.set(k, [])
    groups.get(k)!.push(ms(c))
  }
  const order = ['warm', 'restore', 'boot']
  const kinds = [...groups.keys()].sort((a, b) => {
    const ia = order.indexOf(a) < 0 ? 99 : order.indexOf(a)
    const ib = order.indexOf(b) < 0 ? 99 : order.indexOf(b)
    return ia - ib || a.localeCompare(b)
  })
  return kinds.map((k) => {
    const v = groups.get(k)!
    return { kind: k, count: v.length, p50: percentile(v, 50), p99: percentile(v, 99) }
  })
}

export function fmtMs(ms: number): string {
  if (!Number.isFinite(ms)) return '–'
  if (ms < 10) return `${ms.toFixed(1)} ms`
  if (ms < 10_000) return `${Math.round(ms)} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

// ---------------------------------------------------------------- latency chart y axis

/** The latency chart's y range: 'auto' fits the slowest call shown, the others are fixed tops. */
export type YRange = 'auto' | 2_000 | 10_000 | 60_000
export const Y_RANGES: { value: YRange; label: string }[] = [
  { value: 'auto', label: 'Auto' },
  { value: 2_000, label: '2s' },
  { value: 10_000, label: '10s' },
  { value: 60_000, label: '1m' },
]
/** Steps the automatic top snaps to (ms). A call can run up to 60 s plus VM start and upload. */
const AUTO_TOPS = [2_000, 5_000, 10_000, 30_000, 60_000, 120_000]
export const Y_BOTTOM = 0.5

/** yTop is the top of the y axis: the fixed range, or for 'auto' the first step above the
 * slowest call (with 20 % headroom so its dot is not cut in half), at least 2 s. */
export function yTop(range: YRange, maxMs: number): number {
  if (range !== 'auto') return range
  const want = maxMs * 1.2
  return AUTO_TOPS.find((t) => t >= want) ?? AUTO_TOPS[AUTO_TOPS.length - 1]
}

/** yTicks are the log-scale ticks (1-5-10 steps, then whole seconds) that fit under top. */
export function yTicks(top: number): number[] {
  return [1, 5, 10, 50, 100, 500, 1_000, 5_000, 10_000, 30_000, 60_000, 120_000].filter((t) => t <= top)
}

/** fmtAxisMs labels a y tick: 500, 1s, 30s, 1m. */
export function fmtAxisMs(v: number): string {
  if (v >= 60_000) return `${v / 60_000}m`
  if (v >= 1_000) return `${v / 1_000}s`
  return `${v}`
}

export function fmtBytes(b: number): string {
  if (!Number.isFinite(b) || b <= 0) return '–'
  const mib = b / (1 << 20)
  if (mib < 1024) return `${mib < 10 ? mib.toFixed(1) : Math.round(mib)} MiB`
  return `${(mib / 1024).toFixed(1)} GiB`
}

export function fmtAge(fromIso: string, now: number): string {
  const s = Math.max(0, (now - Date.parse(fromIso)) / 1000)
  if (s < 60) return `${Math.floor(s)}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${Math.floor(s % 60)}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}
