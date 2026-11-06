import { useCallback, useEffect, useMemo, useReducer, useState } from 'react'
import { ApiError, Client, type Fn, type Hello, type InvokeEvent, type NodeInfo, type VM } from './api'
import { connectEvents, type StreamStatus } from './sse'
import { callsPerSecond, initialState, prunePulse, reduce } from './state'
import { forgetKey, loadKey, saveKey } from './storage'
import { KeyForm } from './components/KeyForm'
import { Density } from './components/Density'
import { VmGrid } from './components/VmGrid'
import { InvokePanel } from './components/InvokePanel'
import { LatencyChart } from './components/LatencyChart'
import { PoolTable } from './components/PoolTable'

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
  const [now, setNow] = useState(Date.now())

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
            if (!(data as VM).function) loadFunctions()
          } else if (m.event === 'invoke') {
            dispatch({ type: 'invoke', call: data as InvokeEvent, now: t })
          }
        } catch {
          // ignore a broken message
        }
      },
    })
  }, [client, onSignOut, loadFunctions])

  // Deploys are not events, and node memory is read on request: poll those.
  useEffect(() => {
    loadFunctions()
    const f = setInterval(loadFunctions, 10_000)
    const loadNode = () => client.node().then(setNode, onError)
    loadNode()
    const n = setInterval(loadNode, 2_000)
    return () => {
      clearInterval(f)
      clearInterval(n)
    }
  }, [client, loadFunctions, onError])

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
        <InvokePanel client={client} functions={functions} onError={onError} />
      </div>

      <div className="row row-main row-bottom">
        <LatencyChart calls={live.calls} now={now} />
        <PoolTable vms={vms} functions={functions} calls={live.calls} client={client} onError={onError} />
      </div>

      <footer className="muted foot">
        Uses only the public API: <code>/api/v1/events</code> (live), <code>/api/v1/vms</code>,{' '}
        <code>/api/v1/node</code>, <code>/api/v1/functions</code>, <code>/invoke</code>. Shows your tenant's VMs; the
        node counters include every tenant.
      </footer>
    </div>
  )
}
