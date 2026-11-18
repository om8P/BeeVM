import { useCallback, useEffect, useState } from 'react'
import { ApiError, type Client, type Snapshot, type SnapshotList } from '../api'
import { fmtAge, fmtBytes, fmtMs } from '../stats'

interface Props {
  client: Client
  now: number
  onError: (e: unknown) => void
}

// Snapshots change in the background (built after deploys, rebuilt after deletes), so the list
// is read again every few seconds.
const REFRESH_MS = 5_000

// SnapshotPanel: the node's runtime snapshots and your function snapshots, each deletable on
// its own. Runtime snapshots are shared by every tenant, so deleting one needs the server's
// --allow-runtime-snapshot-delete; function snapshots are only ever your own.
export function SnapshotPanel({ client, now, onError }: Props) {
  const [list, setList] = useState<SnapshotList | null>(null)
  const [unavailable, setUnavailable] = useState('')
  const [busy, setBusy] = useState('')
  const [msg, setMsg] = useState('')

  const load = useCallback(() => {
    client.snapshots().then(
      (l) => {
        setList(l)
        setUnavailable('')
      },
      (e) => {
        onError(e)
        // Never leave the panel on "Loading…": say what is wrong. A 404 means the server does
        // not know the route: it is older than this page (the page is read from web/dist on
        // every request, the server's code only changes when it is restarted).
        if (e instanceof ApiError && e.status === 501) setUnavailable(e.message)
        else if (e instanceof ApiError && e.status === 404)
          setUnavailable('This server is older than the dashboard and has no snapshot API. Restart it with the new build: make build, then beevmd serve.')
        else if (e instanceof ApiError && e.status === 0) setUnavailable('Could not reach the server.')
        else setUnavailable(`Could not load the snapshots: ${e instanceof Error ? e.message : String(e)}`)
      },
    )
  }, [client, onError])

  useEffect(() => {
    load()
    const t = setInterval(load, REFRESH_MS)
    return () => clearInterval(t)
  }, [load])

  const remove = async (s: Snapshot) => {
    const what =
      s.kind === 'runtime'
        ? `Delete the ${s.runtime} runtime snapshot (${s.memory_mib} MiB, ${s.vcpus} vCPU)?\n\nEvery tenant shares it: ` +
          `the next cold start of a ${s.runtime} function waits about 2-3 s while it is rebuilt.`
        : `Delete the function snapshot of ${s.function || 'a removed function'}?\n\nIts next cold start restores the ` +
          `${s.runtime} runtime snapshot and imports the code instead (a little slower); that call builds it again.`
    if (!window.confirm(what)) return
    setBusy(s.key)
    setMsg('')
    try {
      await client.deleteSnapshot(s.key)
      setMsg(`Deleted ${s.kind} snapshot ${s.key}.`)
      load()
    } catch (e) {
      onError(e)
      setMsg(e instanceof ApiError ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  return (
    <section className="card panel snapshots" aria-label="Snapshots">
      <div className="panel-head">
        <h2>Snapshots</h2>
        <span className="muted small">frozen VMs that new VMs are restored from</span>
      </div>
      {unavailable ? (
        <p className="muted empty">{unavailable}</p>
      ) : !list ? (
        <p className="muted empty">Loading…</p>
      ) : (
        <div className="snap-grid">
          <div>
            <h3>Runtime snapshots</h3>
            <p className="muted small snap-help">
              One per runtime and VM size, shared by every tenant on this node.
              {!list.runtime_delete_allowed && (
                <>
                  {' '}
                  Deleting them is off on this server (operator: <code>beevmd snapshot rm</code>, or{' '}
                  <code>serve --allow-runtime-snapshot-delete</code>).
                </>
              )}
            </p>
            <SnapTable
              rows={list.runtime}
              first={(s) => s.runtime}
              firstLabel="runtime"
              canDelete={list.runtime_delete_allowed}
              disabledWhy="Runtime snapshots are shared by every tenant; this server does not let tenants delete them."
              busy={busy}
              now={now}
              onDelete={remove}
            />
          </div>
          <div>
            <h3>Function snapshots</h3>
            <p className="muted small snap-help">Yours only: one per function, with its code already imported.</p>
            <SnapTable
              rows={list.function}
              first={(s) => s.function || '(removed)'}
              firstLabel="function"
              canDelete
              disabledWhy=""
              busy={busy}
              now={now}
              onDelete={remove}
            />
          </div>
        </div>
      )}
      {msg && (
        <p className="muted small" role="status">
          {msg}
        </p>
      )}
    </section>
  )
}

interface TableProps {
  rows: Snapshot[]
  first: (s: Snapshot) => string
  firstLabel: string
  canDelete: boolean
  disabledWhy: string
  busy: string
  now: number
  onDelete: (s: Snapshot) => void
}

function SnapTable({ rows, first, firstLabel, canDelete, disabledWhy, busy, now, onDelete }: TableProps) {
  if (rows.length === 0) return <p className="muted small">None yet.</p>
  return (
    <div className="table-scroll">
      <table className="snaps">
        <thead>
          <tr>
            <th scope="col">{firstLabel}</th>
            {firstLabel === 'function' && <th scope="col">runtime</th>}
            <th scope="col">VM</th>
            <th scope="col" className="num">
              size
            </th>
            <th scope="col" className="num">
              built
            </th>
            <th scope="col">status</th>
            <th scope="col">
              <span className="sr-only">actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((s) => (
            <tr key={s.key}>
              <th scope="row" title={s.key}>
                <span className="fn-name">{first(s)}</span>
              </th>
              {firstLabel === 'function' && <td>{s.runtime}</td>}
              <td className="nowrap">
                {s.memory_mib} MiB · {s.vcpus} vCPU{s.isolation ? ` · ${s.isolation}` : ''}
              </td>
              <td className="num">{fmtBytes(s.size_bytes)}</td>
              <td className="num nowrap" title={`built in ${fmtMs(s.build_ms)} on ${new Date(s.created_at).toLocaleString()}`}>
                {fmtAge(s.created_at, now)} ago
              </td>
              <td>
                <span className={`badge ${s.current ? 'ok' : 'stale'}`}>{s.current ? 'current' : 'stale'}</span>
              </td>
              <td>
                <button
                  className="small-btn ghost danger"
                  disabled={!canDelete || busy === s.key}
                  title={canDelete ? `Delete snapshot ${s.key}` : disabledWhy}
                  aria-label={`Delete ${s.kind} snapshot ${first(s)}`}
                  onClick={() => onDelete(s)}
                >
                  {busy === s.key ? '…' : 'Delete'}
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
