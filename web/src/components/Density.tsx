import type { NodeInfo } from '../api'
import { fmtBytes } from '../stats'

// Density: how full this node is. These are stat tiles, not a chart: each is one number.
export function Density({ node }: { node: NodeInfo | null }) {
  const n = node
  const pssPerVM = n && n.measured_vms > 0 ? n.pss_bytes / n.measured_vms : NaN
  const rssPerVM = n && n.measured_vms > 0 ? n.rss_bytes / n.measured_vms : NaN
  // How many more VMs like the current ones would fit: by free slots, and by free memory.
  const slotRoom = n ? Math.max(0, n.max_vms - n.vms) : 0
  const memRoom = n && Number.isFinite(pssPerVM) && pssPerVM > 0 ? Math.floor(n.mem_available_bytes / pssPerVM) : NaN
  const fill = n && n.max_vms > 0 ? Math.min(1, n.vms / n.max_vms) : 0

  return (
    <section className="tiles" aria-label="Node density">
      <div className="card tile">
        <div className="tile-label">VMs on this node</div>
        <div className="tile-value num">
          {n ? n.vms : '–'}
          <span className="tile-of"> / {n && n.max_vms > 0 ? n.max_vms : '–'}</span>
        </div>
        <div className="meter" role="meter" aria-valuemin={0} aria-valuemax={n?.max_vms ?? 0} aria-valuenow={n?.vms ?? 0}>
          <div className="meter-fill" style={{ width: `${fill * 100}%` }} />
        </div>
        <div className="tile-sub muted">
          {n ? `${n.states.idle ?? 0} idle · ${n.states.busy ?? 0} busy · ${n.states.restoring ?? 0} starting` : ''}
          {n && n.vms > n.tenant_vms ? ` · ${n.vms - n.tenant_vms} of other tenants` : ''}
        </div>
      </div>
      <div className="card tile">
        <div className="tile-label">Memory per VM (PSS)</div>
        <div className="tile-value num">{fmtBytes(pssPerVM)}</div>
        <div className="tile-sub muted" title="RSS counts shared snapshot pages in every VM; PSS splits them between the VMs sharing them.">
          RSS {fmtBytes(rssPerVM)} · measured {n?.measured_vms ?? 0} VMs
        </div>
      </div>
      <div className="card tile">
        <div className="tile-label">Node memory available</div>
        <div className="tile-value num">{fmtBytes(n?.mem_available_bytes ?? NaN)}</div>
        <div className="tile-sub muted">of {fmtBytes(n?.mem_total_bytes ?? NaN)}</div>
      </div>
      <div className="card tile">
        <div className="tile-label">Room for more VMs</div>
        <div className="tile-value num">{n ? slotRoom : '–'}</div>
        <div className="tile-sub muted">
          free slots · memory would fit ≈ {Number.isFinite(memRoom) ? memRoom.toLocaleString() : '–'} more
        </div>
      </div>
    </section>
  )
}
