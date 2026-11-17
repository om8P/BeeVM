import { lazy, Suspense, useCallback, useEffect, useMemo, useReducer, useState } from 'react'
import {
  ApiError,
  Client,
  type Fn,
  type FunctionEvent,
  type Hello,
  type InvokeEvent,
  type NodeInfo,
  type VM,
} from './api'
import { connectEvents, type StreamStatus } from './sse'
import { applyFunctionEvent, callsPerSecond, initialState, prunePulse, reduce } from './state'
import { forgetKey, loadKey, saveKey } from './storage'
import { KeyForm } from './components/KeyForm'
import { Density } from './components/Density'
import { VmGrid } from './components/VmGrid'
import { InvokePanel } from './components/InvokePanel'
import { DeployPanel } from './components/DeployPanel'
import { PoolTable } from './components/PoolTable'

// The chart library (Recharts) is most of the JavaScript. Loading the chart lazily puts it in a
// separate file that the browser fetches after the rest of the page is already working.
const LatencyChart = lazy(() => import('./components/LatencyChart').then((m) => ({ default: m.LatencyChart })))

export function App() {
  const [key, setKey] = useState(loadKey)
  const [authError, setAuthError] = useState('')

  const signOut = useCallback((reason = '') => {
    forgetKey()
    setKey('')
    setAuthError(reason)
  }, [])

  if (!key) {
    return (
      <KeyForm
        error={authError}
        onKey={(k, remember) => {
          saveKey(k, remember)
          setAuthError('')
          setKey(k)
        }}
      />
    )
  }
  return <Dashboard key={key} apiKey={key} onSignOut={signOut} />
}

function Dashboard({ apiKey, onSignOut }: { apiKey: string; onSignOut: (reason?: string) => void }) {
  const client = useMemo(() => new Client(apiKey), [apiKey])
  const [live, dispatch] = useReducer(reduce, initialState)
  const [status, setStatus] = useState<StreamStatus>('connecting')
  const [functions, setFunctions] = useState<Fn[]>([])
  const [node, setNode] = useState<NodeInfo | null>(null)
  const [tenant, setTenant] = useState('')
  const [now, setNow] = useState(Date.now())
  // "Test it" after a deploy: which function the Invoke panel should select.
  const [pick, setPick] = useState<{ name: string; seq: number } | undefined>()
  const testFunction = useCallback((name: string) => setPick((p) => ({ name, seq: (p?.seq ?? 0) + 1 })), [])

  // Any 401 means the key is wrong or was revoked: back to the key form.
  const onError = useCallback(
    (e: unknown) => {
      if (e instanceof ApiError && e.status === 401) onSignOut('The server did not accept this API key.')
    },
    [onSignOut],
  )

  const loadFunctions = useCallback(() => {
    client.functions().then(setFunctions, onError)
  }, [client, onError])

  // The live event stream: everything on the page that moves comes from here.
  useEffect(() => {
    return connectEvents({
      url: '/api/v1/events',
      headers: client.headers(),
      onStatus: (s) => {
        setStatus(s)
        if (s === 'unauthorized') onSignOut('The server did not accept this API key.')
      },
      onMessage: (m) => {
        const t = Date.now()
        try {
          const data = JSON.parse(m.data)
          if (m.event === 'hello') {
            dispatch({ type: 'hello', hello: data as Hello })
            loadFunctions() // a reconnect may follow deploys we did not see
          } else if (m.event === 'vm') {
            dispatch({ type: 'vm', vm: data as VM, now: t })
          } else if (m.event === 'function') {
            // A deploy or delete (also from the CLI): no need to poll the function list.
            setFunctions((list) => applyFunctionEvent(list, data as FunctionEvent))
          } else if (m.event === 'invoke') {
            dispatch({ type: 'invoke', call: data as InvokeEvent, now: t })
          }
        } catch {
          // ignore a broken message
        }
      },
    })
  }, [client, onSignOut, loadFunctions])

  // Who we are (once), and node memory, which is read on request: poll that one. The function
  // list comes with the stream's hello and is then kept up to date by "function" events.
  useEffect(() => {
    client.me().then((m) => setTenant(m.tenant), onError)
    const loadNode = () => client.node().then(setNode, onError)
    loadNode()
    const n = setInterval(loadNode, 2_000)
    return () => clearInterval(n)
  }, [client, onError])

  // A clock for ages and tile flashes.
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 250)
    return () => clearInterval(t)
  }, [])
  const pulse = useMemo(() => prunePulse(live.pulse, now), [live.pulse, now])

  const vms = useMemo(() => Object.values(live.vms), [live.vms])
  const rate = callsPerSecond(live.calls, now)

  return (
    <div className="page">
      <header className="top">
        <div className="brand">
          <svg viewBox="0 0 32 32" width="22" height="22" aria-hidden="true">
            <path d="M16 2l12 7v14l-12 7-12-7V9z" fill="var(--accent)" />
          </svg>
          <span>BeeVM</span>
          <span className="muted">live dashboard</span>
          {tenant && (
            <span className="tenant" title="The tenant this API key belongs to">
              tenant <strong>{tenant}</strong>
            </span>
          )}
        </div>
        <div className="top-right">
          <span className={`pill pill-${status}`} title="Event stream (GET /api/v1/events)">
            <span className="dot" />
            {status === 'live' ? 'live' : 'reconnecting…'}
          </span>
          <span className="muted num">{rate.toFixed(1)} calls/s</span>
          <button className="ghost" onClick={() => onSignOut()}>
            Forget key
          </button>
        </div>
      </header>

      <Density node={node} />

      <div className="row row-main">
        <VmGrid vms={vms} functions={functions} node={node} maxVMs={live.maxVMs} pulse={pulse} now={now} />
        <div className="stack">
          <InvokePanel client={client} functions={functions} onError={onError} pick={pick} />
          <DeployPanel client={client} functions={functions} onError={onError} onTest={testFunction} />
        </div>
      </div>

      <div className="row row-main row-bottom">
        <Suspense
          fallback={
            <section className="card panel chart">
              <h2>Latency: cold vs warm</h2>
              <p className="muted empty">Loading the chart…</p>
            </section>
          }
        >
          <LatencyChart calls={live.calls} functions={functions} now={now} />
        </Suspense>
        <PoolTable vms={vms} functions={functions} calls={live.calls} client={client} onError={onError} />
      </div>

      <footer className="muted foot">
        Uses only the public API: <code>/api/v1/events</code> (live), <code>/api/v1/vms</code>,{' '}
        <code>/api/v1/node</code>, <code>/api/v1/functions</code> (list, deploy, delete), <code>/invoke</code>. Shows your tenant's VMs; the
        node counters include every tenant.
      </footer>
    </div>
  )
}
