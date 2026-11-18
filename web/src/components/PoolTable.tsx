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

  // remove deletes a function after asking; the "function" event then drops it from every open
  // dashboard (this one included), so nothing else needs updating here.
  const remove = async (name: string) => {
    if (!window.confirm(`Delete the function ${name}? Its VMs and snapshot are removed too. This cannot be undone.`)) return
    setBusy(name)
    setMsg((m) => ({ ...m, [name]: '' }))
    try {
      await client.deleteFunction(name)
    } catch (e) {
      onError(e)
      setMsg((m) => ({ ...m, [name]: e instanceof ApiError ? e.message : String(e) }))
    } finally {
      setBusy('')
    }
  }

  // cool throws away warm VMs (one function, or all with name ''), so the next call is a cold
  // start again; the VM tiles and counts update through the event stream.
  const [coolMsg, setCoolMsg] = useState('')
  const warmVMs = vms.filter((v) => v.state !== 'destroyed').length
  const cool = async (name: string) => {
    setBusy(name || '*')
    setCoolMsg('')
    if (name) setMsg((m) => ({ ...m, [name]: '' }))
    // How many VMs the function has right now, to say honestly whether the next call is cold.
    const p = byName.get(name)
    const had = name ? (p?.idle ?? 0) + (p?.busy ?? 0) + (p?.restoring ?? 0) : warmVMs
    try {
      // One function: as many as the row's number says (the same box as Prewarm).
      const n = await client.cool(name || undefined, name ? (count[name] ?? 1) : undefined)
      const left = had - n
      const text =
        n === 0
          ? 'no warm VMs: next call is cold'
          : left > 0
            ? `cooled ${n} VM${n === 1 ? '' : 's'}, ${left} still warm`
            : `cooled ${n} VM${n === 1 ? '' : 's'}: next call is cold`
      if (name) setMsg((m) => ({ ...m, [name]: text }))
      else setCoolMsg(text)
    } catch (e) {
      onError(e)
      const text = e instanceof ApiError && e.status === 501 ? 'no warm pool on this server' : e instanceof ApiError ? e.message : String(e)
      if (name) setMsg((m) => ({ ...m, [name]: text }))
      else setCoolMsg(text)
    } finally {
      setBusy('')
    }
  }

  return (
    <section className="card panel" aria-label="Warm pools">
      <div className="panel-head">
        <h2>Warm pools</h2>
        {functions.length > 0 && (
          <span className="cool-all">
            {coolMsg && <span className="muted small">{coolMsg}</span>}
            <button
              className="small-btn ghost"
              disabled={busy === '*' || warmVMs === 0}
              title="Throw away all your warm VMs, so every function's next call is a cold start"
              onClick={() => cool('')}
            >
              {busy === '*' ? '…' : 'Cool all'}
            </button>
          </span>
        )}
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
              <th scope="col">actions</th>
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
                        aria-label={`VMs to prewarm or cool for ${f.name}`}
                        title="How many VMs Prewarm starts and Cool throws away"
                        value={count[f.name] ?? 1}
                        onChange={(e) => setCount((c) => ({ ...c, [f.name]: Math.max(1, Number(e.target.value) || 1) }))}
                      />
                      <button className="small-btn" disabled={busy === f.name} onClick={() => prewarm(f.name)}>
                        {busy === f.name ? '…' : 'Prewarm'}
                      </button>
                      <button
                        className="small-btn ghost"
                        disabled={busy === f.name || ((p?.idle ?? 0) + (p?.busy ?? 0) + (p?.restoring ?? 0)) === 0}
                        onClick={() => cool(f.name)}
                        aria-label={`Cool ${f.name}`}
                        title={`Throw away ${count[f.name] ?? 1} of ${f.name}'s warm VMs (idle first), so the next call is a cold start`}
                      >
                        Cool
                      </button>
                      <button
                        className="small-btn ghost danger"
                        disabled={busy === f.name}
                        onClick={() => remove(f.name)}
                        aria-label={`Delete ${f.name}`}
                        title={`Delete ${f.name}`}
                      >
                        Delete
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
