import { useEffect, useState } from 'react'
import { ApiError, type Client, type Fn, type InvokeResult } from '../api'
import { fmtMs } from '../stats'

interface Props {
  client: Client
  functions: Fn[]
  onError: (e: unknown) => void
}

// InvokePanel: call a function and see where the time went.
export function InvokePanel({ client, functions, onError }: Props) {
  const [name, setName] = useState('')
  const [payload, setPayload] = useState('{"name":"BeeVM"}')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<InvokeResult | null>(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    if (!functions.some((f) => f.name === name)) setName(functions[0]?.name ?? '')
  }, [functions, name])

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
    <section className="card panel invoke" aria-label="Invoke">
      <div className="panel-head">
        <h2>Invoke</h2>
      </div>
      {functions.length === 0 ? (
        <p className="muted empty">
          No functions yet. Deploy one: <code>beevm deploy hello bin/functions/hello-go-binary</code>
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
          <button onClick={invoke} disabled={busy || !name}>
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
