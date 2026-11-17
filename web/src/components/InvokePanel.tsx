import { useEffect, useRef, useState } from 'react'
import { ApiError, type Client, type Fn, type InvokeResult } from '../api'
import { fmtMs } from '../stats'

interface Props {
  client: Client
  functions: Fn[]
  onError: (e: unknown) => void
  // pick (optional) selects a function from outside, e.g. "Test it" after a deploy; seq makes
  // picking the same name twice count again.
  pick?: { name: string; seq: number }
}

// InvokePanel: call a function and see where the time went.
export function InvokePanel({ client, functions, onError, pick }: Props) {
  const [name, setName] = useState('')
  const [payload, setPayload] = useState('{"name":"BeeVM"}')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<InvokeResult | null>(null)
  const [err, setErr] = useState('')
  const panel = useRef<HTMLElement>(null)
  const button = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    if (!functions.some((f) => f.name === name)) setName(functions[0]?.name ?? '')
  }, [functions, name])

  // Picked from outside: select it (once the function list has it), bring the panel into view
  // and put the focus on Invoke.
  const pickSeq = pick?.seq
  const pickName = pick?.name
  const known = !!pickName && functions.some((f) => f.name === pickName)
  useEffect(() => {
    if (!pickName || !known) return
    setName(pickName)
    setResult(null)
    setErr('')
    panel.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest' })
    button.current?.focus({ preventScroll: true })
  }, [pickSeq, pickName, known])

  const invoke = async () => {
    setBusy(true)
    setErr('')
    try {
      JSON.parse(payload) // say it here instead of a 400 from the server
    } catch {
      setErr('The payload is not valid JSON.')
      setBusy(false)
      return
    }
    try {
      setResult(await client.invoke(name, payload))
    } catch (e) {
      onError(e)
      setResult(null)
      setErr(e instanceof ApiError ? `${e.status}: ${e.message}` : String(e))
    } finally {
      setBusy(false)
    }
  }

  const t = result?.timing
  // A warm call reuses a running VM: there is no VM start, upload or import to show.
  const cold = t?.start !== 'warm'
  const rows: [string, number][] = t
    ? [
        ...(cold
          ? ([
              ['VM ready (cold start)', t.bootMs],
              ['upload code', t.uploadMs],
              ['import code', t.loadMs],
            ] as [string, number][])
          : []),
        ['function', t.functionMs],
        ['server total', t.totalMs],
        ['round trip (browser)', result!.roundTripMs],
      ]
    : []

  return (
    <section ref={panel} className="card panel invoke" aria-label="Invoke">
      <div className="panel-head">
        <h2>Invoke</h2>
      </div>
      {functions.length === 0 ? (
        <p className="muted empty">
          No functions yet. Deploy one with the Deploy panel, or <code>beevm deploy hello bin/functions/hello-go-binary</code>
        </p>
      ) : (
        <>
          <label htmlFor="fn">Function</label>
          <select id="fn" value={name} onChange={(e) => setName(e.target.value)}>
            {functions.map((f) => (
              <option key={f.name} value={f.name}>
                {f.name} ({f.runtime || 'binary'}, {f.memory_mib} MiB)
              </option>
            ))}
          </select>
          <label htmlFor="payload">Payload (JSON)</label>
          <textarea id="payload" rows={3} spellCheck={false} value={payload} onChange={(e) => setPayload(e.target.value)} />
          <button ref={button} onClick={invoke} disabled={busy || !name}>
            {busy ? 'Calling…' : 'Invoke'}
          </button>
        </>
      )}

      {err && (
        <p className="error" role="alert">
          {err}
        </p>
      )}

      {result && t && (
        <div className="result">
          <div className="result-head">
            <span className={`badge ${result.ok ? 'ok' : 'bad'}`}>{result.status}</span>
            {t.start && (
              <span className="start-kind">
                <span className={`swatch sk-${t.start}`} />
                {t.start}
                {t.snapshot ? ` from ${t.snapshot} snapshot` : ''}
              </span>
            )}
            <span className="muted">{t.vm}</span>
          </div>
          <table className="timing">
            <tbody>
              {rows.map(([label, ms]) => (
                <tr key={label}>
                  <th scope="row">{label}</th>
                  <td className="num">{fmtMs(ms)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <pre className="output">{pretty(result.body)}</pre>
        </div>
      )}
    </section>
  )
}

function pretty(body: string): string {
  try {
    return JSON.stringify(JSON.parse(body), null, 2)
  } catch {
    return body
  }
}
