import { useState, type FormEvent } from 'react'
import { ApiError, Client } from '../api'

// KeyForm asks for a tenant API key (made with `beevmd tenant add <name>`) and checks it once
// before the dashboard opens.
export function KeyForm({ error, onKey }: { error: string; onKey: (key: string, remember: boolean) => void }) {
  const [key, setKey] = useState('')
  const [remember, setRemember] = useState(false)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState(error)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const k = key.trim()
    if (!k) return
    setBusy(true)
    setMsg('')
    try {
      await new Client(k).functions()
      onKey(k, remember)
    } catch (err) {
      setMsg(
        err instanceof ApiError && err.status === 401
          ? 'The server did not accept this API key.'
          : `Cannot reach beevmd: ${err instanceof Error ? err.message : String(err)}`,
      )
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="keyform-wrap">
      <form className="card keyform" onSubmit={submit}>
        <h1>BeeVM dashboard</h1>
        <p className="muted">
          Paste a tenant API key. Create one on the server with <code>./bin/beevmd tenant add &lt;name&gt;</code>.
        </p>
        <label htmlFor="key">API key</label>
        <input
          id="key"
          type="password"
          autoComplete="off"
          spellCheck={false}
          placeholder="bvm_…"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          autoFocus
        />
        <label className="check">
          <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} />
          Remember on this computer
        </label>
        {msg && (
          <p className="error" role="alert">
            {msg}
          </p>
        )}
        <button type="submit" disabled={busy || !key.trim()}>
          {busy ? 'Checking…' : 'Open dashboard'}
        </button>
      </form>
    </div>
  )
}
