import { useMemo, useState } from 'react'
import {
  CartesianGrid,
  ReferenceLine,
  ResponsiveContainer,
  Scatter,
  ScatterChart,
  Tooltip,
  XAxis,
  YAxis,
  ZAxis,
} from 'recharts'
import type { Call } from '../state'
import { fmtMs, summarize } from '../stats'

// The three start kinds, in the fixed order (and colours) used everywhere on the page.
const KINDS = [
  { kind: 'warm', label: 'warm (VM reused)' },
  { kind: 'restore', label: 'restore (snapshot)' },
  { kind: 'boot', label: 'boot (no snapshot)' },
]
const WINDOWS = [60, 120, 300]

interface Point {
  x: number // seconds before now (negative)
  y: number // server time in ms (at least 0.1, for the log scale)
  call: Call
}

// LatencyChart: every call of the last minutes as a dot, by how its VM started. The y axis is
// logarithmic so a 1 ms warm call and a 1 s boot both stay readable.
export function LatencyChart({ calls, now }: { calls: Call[]; now: number }) {
  const [win, setWin] = useState(120)
  const recent = useMemo(() => calls.filter((c) => c.t >= now - win * 1000 && c.start), [calls, now, win])
  const series = useMemo(
    () =>
      KINDS.map((k) => ({
        ...k,
        points: recent
          .filter((c) => c.start === k.kind)
          .map((c): Point => ({ x: (c.t - now) / 1000, y: Math.max(0.1, c.duration_ms), call: c })),
      })),
    [recent, now],
  )
  const summary = useMemo(() => summarize(recent, (c) => c.start ?? '', (c) => c.duration_ms), [recent])
  const coldVMReady = useMemo(() => {
    const cold = recent.filter((c) => c.start === 'restore' || c.start === 'boot')
    return summarize(cold, (c) => c.start ?? '', (c) => c.boot_ms)
  }, [recent])

  return (
    <section className="card panel chart" aria-label="Latency, cold vs warm">
      <div className="panel-head">
        <h2>Latency: cold vs warm</h2>
        <div className="seg" role="group" aria-label="Time window">
          {WINDOWS.map((w) => (
            <button key={w} className={w === win ? 'on' : ''} onClick={() => setWin(w)}>
              {w < 120 ? `${w}s` : `${w / 60}m`}
            </button>
          ))}
        </div>
      </div>
      <ul className="legend" aria-label="Start kinds">
        {KINDS.map((k) => (
          <li key={k.kind}>
            <span className={`swatch round sk-${k.kind}`} />
            {k.label}
          </li>
        ))}
        <li>
          <span className="swatch line-ref" />
          50 ms cold-start target
        </li>
      </ul>

      <div className="chart-box">
        <div className="axis-title muted">server time per call, ms (log scale)</div>
        {recent.length === 0 ? (
          <p className="muted empty">No calls in the last {win} s. Invoke a function or run a load test.</p>
        ) : (
          <ResponsiveContainer width="100%" height={260}>
            <ScatterChart margin={{ top: 8, right: 16, bottom: 4, left: 4 }}>
              <CartesianGrid stroke="var(--grid)" vertical={false} />
              <XAxis
                type="number"
                dataKey="x"
                domain={[-win, 0]}
                tickCount={7}
                tickFormatter={(v: number) => (v === 0 ? 'now' : `${Math.round(v)}s`)}
                stroke="var(--axis)"
                tick={{ fill: 'var(--muted)', fontSize: 12 }}
              />
              <YAxis
                type="number"
                dataKey="y"
                scale="log"
                domain={[0.5, 2000]}
                allowDataOverflow
                ticks={[1, 5, 10, 50, 100, 500, 1000]}
                tickFormatter={(v: number) => (v >= 1000 ? `${v / 1000}s` : `${v}`)}
                stroke="var(--axis)"
                tick={{ fill: 'var(--muted)', fontSize: 12 }}
                width={44}
              />
              <ZAxis range={[64, 64]} />
              <ReferenceLine y={50} stroke="var(--text-2)" strokeDasharray="4 4" />
              <Tooltip content={(p) => <Tip active={p.active} payload={p.payload} />} cursor={false} isAnimationActive={false} />
              {series.map((s) => (
                <Scatter
                  key={s.kind}
                  name={s.label}
                  data={s.points}
                  fill={`var(--sk-${s.kind})`}
                  stroke="var(--surface)"
                  strokeWidth={1.5}
                  isAnimationActive={false}
                />
              ))}
            </ScatterChart>
          </ResponsiveContainer>
        )}
      </div>

      <table className="summary">
        <caption className="sr-only">Server time per start kind, last {win} seconds</caption>
        <thead>
          <tr>
            <th scope="col">start</th>
            <th scope="col" className="num">
              calls
            </th>
            <th scope="col" className="num">
              p50
            </th>
            <th scope="col" className="num">
              p99
            </th>
            <th scope="col" className="num" title="Time until the new VM's agent answered (the cold start itself)">
              VM ready p50 / p99
            </th>
          </tr>
        </thead>
        <tbody>
          {summary.length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
                –
              </td>
            </tr>
          )}
          {summary.map((s) => {
            const ready = coldVMReady.find((c) => c.kind === s.kind)
            return (
              <tr key={s.kind}>
                <th scope="row">
                  <span className={`swatch round sk-${s.kind}`} />
                  {s.kind}
                </th>
                <td className="num">{s.count}</td>
                <td className="num">{fmtMs(s.p50)}</td>
                <td className="num">{fmtMs(s.p99)}</td>
                <td className="num">{ready ? `${fmtMs(ready.p50)} / ${fmtMs(ready.p99)}` : '–'}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
      <p className="muted note">
        Server time of each call (inside beevmd). p99 needs about 100 calls to mean much.
      </p>
    </section>
  )
}

function Tip({ active, payload }: { active?: boolean; payload?: ReadonlyArray<{ payload?: unknown }> }) {
  const p = payload?.[0]?.payload as Point | undefined
  if (!active || !p) return null
  const c = p.call
  return (
    <div className="tip">
      <div className="tip-title">
        <span className={`swatch round sk-${c.start}`} />
        {c.function} · {c.start}
        {c.snapshot ? ` (${c.snapshot} snapshot)` : ''}
      </div>
      <div className="num">server {fmtMs(c.duration_ms)}</div>
      {c.start !== 'warm' && <div className="num">VM ready {fmtMs(c.boot_ms)}</div>}
      {c.upload_ms > 0 && <div className="num">upload {fmtMs(c.upload_ms)}</div>}
      {c.load_ms > 0 && <div className="num">import {fmtMs(c.load_ms)}</div>}
      <div className="num">function {fmtMs(c.function_ms)}</div>
      <div className="muted">
        HTTP {c.status} · {c.vm}
      </div>
    </div>
  )
}
