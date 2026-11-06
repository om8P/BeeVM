import type { Fn, NodeInfo, VM } from '../api'
import { fmtAge } from '../stats'

const STATES = ['restoring', 'idle', 'busy', 'recycling'] as const
const LETTER: Record<string, string> = { restoring: 'R', idle: 'I', busy: 'B', recycling: '×' }

interface Props {
  vms: VM[]
  functions: Fn[]
  node: NodeInfo | null
  maxVMs: number
  pulse: Record<string, number>
  now: number
}

// VmGrid: one tile per VM of this tenant, grouped by function, coloured by state. Free slots
// and other tenants' VMs fill up the rest, so the grid is the node's capacity at a glance.
export function VmGrid({ vms, functions, node, maxVMs, pulse, now }: Props) {
  const runtime = new Map(functions.map((f) => [f.name, f.runtime]))
  const groups = new Map<string, VM[]>()
  for (const v of [...vms].sort((a, b) => a.id.localeCompare(b.id))) {
    const k = v.function || '(removed)'
    if (!groups.has(k)) groups.set(k, [])
    groups.get(k)!.push(v)
  }
  const names = [...groups.keys()].sort()
  const max = node?.max_vms || maxVMs
  // Other tenants and free slots come from the node counters (polled), counted at one moment;
  // our own tiles come from live events, so the two may differ for a second during churn.
  const others = node ? Math.max(0, node.vms - node.tenant_vms) : 0
  const free = Math.max(0, max - Math.max(node?.vms ?? 0, vms.length + others))

  return (
    <section className="card panel" aria-label="MicroVMs">
      <div className="panel-head">
        <h2>MicroVMs</h2>
        <ul className="legend" aria-label="States">
          {STATES.map((s) => (
            <li key={s}>
              <span className={`swatch st-${s}`} />
              {s}
            </li>
          ))}
          <li>
            <span className="swatch st-free" />
            free slot
          </li>
        </ul>
      </div>

      {names.length === 0 && (
        <p className="muted empty">
          No VMs of yours right now. Invoke or prewarm a function: tiles appear as VMs restore, run and wait.
        </p>
      )}

      {names.map((name) => {
        const list = groups.get(name)!
        return (
          <div key={name} className="vm-group">
            <div className="vm-group-head">
              <span className="fn-name">{name}</span>
              <span className="muted">{runtime.get(name) ?? ''}</span>
              <span className="muted num">{list.length} VM{list.length === 1 ? '' : 's'}</span>
            </div>
            <div className="vm-tiles">
              {list.map((v) => {
                const flashing = (pulse[v.id] ?? 0) > now
                return (
                  <div
                    key={v.id}
                    className={`vm st-${v.state}${flashing ? ' flash' : ''}`}
                    title={`${v.id}\n${v.state} for ${fmtAge(v.since, now)}\n${v.calls} calls${v.addr ? `\naddress ${v.addr}` : ''}`}
                  >
                    <span className="vm-letter" aria-hidden="true">
                      {LETTER[v.state] ?? '?'}
                    </span>
                    <span className="vm-calls num">{v.calls}</span>
                    <span className="sr-only">
                      {v.id} {v.state}, {v.calls} calls
                    </span>
                  </div>
                )
              })}
            </div>
          </div>
        )
      })}

      {(others > 0 || free > 0) && (
        <div className="vm-group">
          <div className="vm-group-head">
            <span className="muted">
              Rest of the node: {others > 0 ? `${others} VM${others === 1 ? '' : 's'} of other tenants, ` : ''}
              {free} free slot{free === 1 ? '' : 's'}
            </span>
          </div>
          <div className="vm-tiles">
            {Array.from({ length: Math.min(others, 64) }, (_, i) => (
              <div key={`o${i}`} className="vm st-other" title="another tenant's VM" />
            ))}
            {Array.from({ length: Math.min(free, 64) }, (_, i) => (
              <div key={`f${i}`} className="vm st-free" title="free slot" />
            ))}
          </div>
        </div>
      )}
    </section>
  )
}
