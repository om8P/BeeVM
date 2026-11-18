import { useEffect, useRef, useState } from 'react'
import { ApiError, type Client, type Fn, type InvokeEvent, type InvokeResult } from '../api'
import { collectAsync, fromResult, type Burst, type BurstCall } from '../burst'
import { loadPayload, savePayload } from '../payloads'
import { fmtMs, percentile, summarize } from '../stats'

interface Props {
  client: Client
  functions: Fn[]
  onError: (e: unknown) => void
  // pick (optional) selects a function from outside, e.g. "Test it" after a deploy; seq makes
  // picking the same name twice count again.
  pick?: { name: string; seq: number }
  // events: the invoke events received on the live stream (async results come in here)
  events: InvokeEvent[]
}

// MAX_BURST caps "calls at once" so a typo cannot start thousands of calls (the server queues
// at most 250 per tenant by default).
const MAX_BURST = 200
// Stop waiting for async results after the server's default max age (5 min) plus a bit.
const ASYNC_GIVE_UP_MS = 330_000

const errText = (e: unknown) => (e instanceof ApiError ? `${e.status}: ${e.message}` : String(e))

// InvokePanel: call a function and see where the time went.
//
// One call per click ("calls at once" = 1) is synchronous: the answer carries the timing. The
// button never waits, so spamming it sends overlapping calls; they form one group, summed up
// when the last one answers. A browser keeps only ~6 connections to one server, though, so at
// most ~5 such calls really run at once.
//
// "calls at once" above 1 uses async invoke instead: each call is queued on the server and
// answered with 202 at once, so all of them run in parallel (up to the node's VM limit); the
// results arrive as "invoke" events on the live stream, matched by request id.
export function InvokePanel({ client, functions, onError, pick, events }: Props) {
  const [name, setName] = useState('')
  // Each function has its own payload: the last one typed for it in this browser, else a sample.
  const [payload, setPayload] = useState('{}')
  const [running, setRunning] = useState(0) // synchronous calls sent and not answered yet
  const [result, setResult] = useState<InvokeResult | null>(null)
  const [err, setErr] = useState('')
  const [count, setCount] = useState(1)
  const [burst, setBurst] = useState<Burst | null>(null)
  const [posting, setPosting] = useState(0) // async calls being queued (202 not back yet)
  // The async burst waiting for its results: the ids the server gave, calls it refused, start.
  const [asyncRun, setAsyncRun] = useState<{ ids: string[]; failed: BurstCall[]; t0: number } | null>(null)
  const panel = useRef<HTMLElement>(null)
  const button = useRef<HTMLButtonElement>(null)
  // The calls of the current synchronous group (calls that overlapped), collected outside React
  // state so that answers arriving together never overwrite each other.
  const group = useRef<{ calls: BurstCall[]; result?: InvokeResult; t0: number; open: number } | null>(null)

  useEffect(() => {
    if (!functions.some((f) => f.name === name)) setName(functions[0]?.name ?? '')
  }, [functions, name])

  useEffect(() => {
    if (name) setPayload(loadPayload(name))
  }, [name])

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

  // An async burst is done when every call was sent and every queued one has its event.
  const asyncFound = asyncRun ? collectAsync(events, asyncRun.ids) : null
  const asyncDone = !!asyncRun && !!asyncFound && posting === 0 && asyncFound.found.length >= asyncRun.ids.length
  useEffect(() => {
    if (!asyncDone || !asyncRun || !asyncFound) return
    const end = asyncFound.lastAt || Date.now()
    setBurst({ calls: [...asyncFound.found, ...asyncRun.failed], wallMs: Math.max(0, end - asyncRun.t0), async: true })
    setAsyncRun(null)
  }, [asyncDone, asyncRun, asyncFound])

  // Do not wait forever (e.g. the live stream was down while the results came).
  const asyncT0 = asyncRun?.t0
  const latest = useRef({ events, asyncRun })
  latest.current = { events, asyncRun }
  useEffect(() => {
    if (asyncT0 === undefined) return
    const timer = setTimeout(() => {
      const run = latest.current.asyncRun
      if (!run) return
      const { found } = collectAsync(latest.current.events, run.ids)
      const lost: BurstCall[] = Array.from({ length: run.ids.length - found.length }, () => ({
        error: 'no result seen on the live stream',
      }))
      setBurst({ calls: [...found, ...run.failed, ...lost], wallMs: Date.now() - run.t0, async: true })
      setAsyncRun(null)
    }, ASYNC_GIVE_UP_MS)
    return () => clearTimeout(timer)
  }, [asyncT0])

  const invoke = () => {
    try {
      JSON.parse(payload) // say it here instead of a 400 from the server
    } catch {
      setErr('The payload is not valid JSON.')
      return
    }
    setErr('')
    if (count > 1) invokeAsync()
    else invokeSync()
  }

  const invokeSync = () => {
    if (!group.current) group.current = { calls: [], t0: performance.now(), open: 0 }
    const g = group.current
    g.open++
    setRunning((n) => n + 1)
    client
      .invoke(name, payload)
      .then(
        (r): BurstCall => {
          g.result = r
          return fromResult(r)
        },
        (e): BurstCall => {
          onError(e)
          return { error: errText(e) }
        },
      )
      .then((call) => {
        g.calls.push(call)
        g.open--
        setRunning((n) => n - 1)
        if (g.open > 0) return
        // The group is complete: show it.
        group.current = null
        if (g.calls.length === 1) {
          setBurst(null)
          setResult(g.result ?? null)
          setErr(g.calls[0].error ?? '')
        } else {
          setResult(null)
          setBurst({ calls: g.calls, wallMs: performance.now() - g.t0, async: false })
        }
      })
  }

  const invokeAsync = () => {
    const n = count
    setResult(null)
    setBurst(null)
    setPosting((p) => p + n)
    // Another click while a burst is still running joins it.
    setAsyncRun((prev) => prev ?? { ids: [], failed: [], t0: Date.now() })
    for (let i = 0; i < n; i++) {
      client.invokeAsync(name, payload).then(
        (a) => {
          setAsyncRun((prev) => (prev ? { ...prev, ids: [...prev.ids, a.request_id] } : prev))
          setPosting((p) => p - 1)
        },
        (e) => {
          // 429 = the server's queue (or this tenant's share of it) is full
          setAsyncRun((prev) => (prev ? { ...prev, failed: [...prev.failed, { error: errText(e) }] } : prev))
          setPosting((p) => p - 1)
        },
      )
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
          <textarea
            id="payload"
            rows={3}
            spellCheck={false}
            value={payload}
            onChange={(e) => {
              setPayload(e.target.value)
              savePayload(name, e.target.value)
            }}
          />
          <div className="invoke-row">
            <button ref={button} onClick={invoke} disabled={!name}>
              {count > 1 ? `Invoke ×${count}` : 'Invoke'}
            </button>
            <label className="burst">
              calls at once
              <input
                type="number"
                min={1}
                max={MAX_BURST}
                value={count}
                onChange={(e) => setCount(Math.min(MAX_BURST, Math.max(1, Math.floor(Number(e.target.value)) || 1)))}
              />
            </label>
            {running > 0 && (
              <span className="muted running" aria-live="polite">
                {running} running…
              </span>
            )}
            {asyncRun && (
              <span className="muted running" aria-live="polite">
                {posting > 0 ? `queuing ${posting}…` : `${asyncFound?.found.length ?? 0}/${asyncRun.ids.length} done`}
              </span>
            )}
          </div>
          {count > 1 ? (
            <p className="muted hint">
              Async invoke: each call is queued on the server and answered at once, so all of them run in parallel
              (up to the node&apos;s VM limit). Results arrive on the live stream.
            </p>
          ) : (
            running > 5 && (
              <p className="muted hint">
                A browser keeps at most 6 connections to one server (one is the live stream), so only about 5 single
                calls run at the same time. Set <em>calls at once</em> above 1 to queue them on the server instead.
              </p>
            )
          )}
        </>
      )}

      {err && (
        <p className="error" role="alert">
          {err}
        </p>
      )}

      {burst && <BurstResult burst={burst} />}

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

// BurstResult sums up a burst: how many VMs served it and how each kind of start did.
function BurstResult({ burst }: { burst: Burst }) {
  const ok = burst.calls.filter((c) => !c.error)
  const failed = burst.calls.filter((c) => c.error)
  const perVM = new Map<string, { calls: number; starts: Set<string> }>()
  for (const c of ok) {
    const id = c.vm || '?'
    if (!perVM.has(id)) perVM.set(id, { calls: 0, starts: new Set() })
    const v = perVM.get(id)!
    v.calls++
    if (c.start) v.starts.add(c.start)
  }
  const kinds = summarize(ok, (c) => c.start || '?', (c) => c.ms ?? NaN)
  // sync: measured in the browser; async: inside the server, from queued to finished
  const what = burst.async ? 'in server' : 'round trip'
  const queued = burst.async ? percentile(ok.map((c) => c.queuedMs ?? 0), 50) : NaN
  return (
    <div className="result">
      <div className="result-head">
        <span className={`badge ${failed.length === 0 ? 'ok' : 'bad'}`}>
          {ok.length}/{burst.calls.length}
        </span>
        <span>
          {burst.async ? 'async, ' : ''}
          {perVM.size} VM{perVM.size === 1 ? '' : 's'} used, all done in {fmtMs(burst.wallMs)}
          {burst.async && ok.length > 0 ? `, p50 wait in queue ${fmtMs(queued)}` : ''}
        </span>
      </div>
      <table className="timing">
        <thead>
          <tr>
            <th>start</th>
            <th className="num">calls</th>
            <th className="num">p50 {what}</th>
            <th className="num">p99 {what}</th>
          </tr>
        </thead>
        <tbody>
          {kinds.map((k) => (
            <tr key={k.kind}>
              <th scope="row">
                <span className={`swatch sk-${k.kind}`} />
                {k.kind}
              </th>
              <td className="num">{k.count}</td>
              <td className="num">{fmtMs(k.p50)}</td>
              <td className="num">{fmtMs(k.p99)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <table className="timing">
        <thead>
          <tr>
            <th>VM</th>
            <th className="num">calls</th>
            <th>starts</th>
          </tr>
        </thead>
        <tbody>
          {[...perVM.entries()].map(([id, v]) => (
            <tr key={id}>
              <th scope="row">{id}</th>
              <td className="num">{v.calls}</td>
              <td>{[...v.starts].join(', ')}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {failed.length > 0 && (
        <p className="error" role="alert">
          {failed.length} call{failed.length === 1 ? '' : 's'} failed: {failed[0].error}
        </p>
      )}
    </div>
  )
}

function pretty(body: string): string {
  try {
    return JSON.stringify(JSON.parse(body), null, 2)
  } catch {
    return body
  }
}
