import { useState } from 'react'
import { ApiError, type Client, type Fn, type VM } from '../api'
import { pools } from '../state'
import type { Call } from '../state'

interface Props {
  vms: VM[]
  functions: Fn[]
  calls: Call[]
  client: Client
  onError: (e: unknown) => void
}

// PoolTable: each function's warm pool (VMs by state) with a prewarm button.
export function PoolTable({ vms, functions, calls, client, onError }: Props) {
  const byName = new Map(pools(vms).map((p) => [p.name, p]))
  const seen = new Map<string, number>()
  for (const c of calls) seen.set(c.function, (seen.get(c.function) ?? 0) + 1)
  const [count, setCount] = useState<Record<string, number>>({})
  const [msg, setMsg] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState('')

  const prewarm = async (name: string) => {
    const n = count[name] ?? 1
    setBusy(name)
    setMsg((m) => ({ ...m, [name]: '' }))
    try {
      const r = await client.prewarm(name, n)
      setMsg((m) => ({ ...m, [name]: `started ${r.started} of ${r.requested}` }))
    } catch (e) {
      onError(e)
      const text =
        e instanceof ApiError
          ? e.status === 503
            ? 'node full'
            : e.status === 501
              ? 'no warm pool (--runner warm)'
              : e.message
          : String(e)
      setMsg((m) => ({ ...m, [name]: text }))
    } finally {
      setBusy('')
    }
  }

  return (
    <section className="card panel" aria-label="Warm pools">
      <div className="panel-head">
        <h2>Warm pools</h2>
      </div>
      {functions.length === 0 ? (
        <p className="muted empty">No functions deployed.</p>
      ) : (
        <div className="table-scroll">
        <table className="pools">
          <thead>
            <tr>
              <th scope="col">function</th>
              <th scope="col" className="num">
                <span className="swatch st-idle" />
                idle
              </th>
              <th scope="col" className="num">
                <span className="swatch st-busy" />
                busy
              </th>
              <th scope="col" className="num">
                <span className="swatch st-restoring" />
                starting
              </th>
              <th scope="col" className="num" title="Calls seen since this page opened">
                calls
              </th>
              <th scope="col">prewarm</th>
            </tr>
          </thead>
          <tbody>
            {functions.map((f) => {
              const p = byName.get(f.name)
              return (
                <tr key={f.name}>
                  <th scope="row">
                    <span className="fn-name">{f.name}</span>
                    <span className="muted small"> {f.runtime || 'binary'}</span>
                  </th>
                  <td className="num">{p?.idle ?? 0}</td>
                  <td className="num">{p?.busy ?? 0}</td>
                  <td className="num">{p?.restoring ?? 0}</td>
                  <td className="num">{seen.get(f.name) ?? 0}</td>
                  <td>
                    <div className="prewarm">
                      <input
                        type="number"
                        min={1}
                        max={64}
                        aria-label={`VMs to prewarm for ${f.name}`}
                        value={count[f.name] ?? 1}
                        onChange={(e) => setCount((c) => ({ ...c, [f.name]: Math.max(1, Number(e.target.value) || 1) }))}
                      />
                      <button className="small-btn" disabled={busy === f.name} onClick={() => prewarm(f.name)}>
                        {busy === f.name ? '…' : 'Prewarm'}
                      </button>
                    </div>
                    {msg[f.name] && <div className="muted small">{msg[f.name]}</div>}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
        </div>
      )}
    </section>
  )
}
