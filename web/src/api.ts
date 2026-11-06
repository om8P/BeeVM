// Typed client for beevmd's public REST API. The types mirror internal/apitypes/apitypes.go;
// if you change one side, change the other.

export interface Fn {
  name: string
  runtime: string
  handler?: string
  code_sha256: string
  code_size: number
  memory_mib: number
  vcpus: number
  timeout_ms: number
  created_at: string
  updated_at: string
}

export type VMState = 'restoring' | 'idle' | 'busy' | 'recycling' | 'destroyed'

export interface VM {
  id: string
  function: string
  state: VMState
  since: string
  last_used: string
  calls: number
  addr?: string
  prev_id?: string // only in "vm" events
}

export interface Hello {
  vms: VM[]
  max_vms: number
}

export interface InvokeEvent {
  function: string
  runtime: string
  status: number
  start?: string // warm, restore, boot
  snapshot?: string // function, runtime
  vm?: string
  boot_ms: number
  upload_ms: number
  load_ms: number
  function_ms: number
  duration_ms: number
  at: string
}

export interface NodeInfo {
  max_vms: number
  vms: number
  tenant_vms: number // of vms, the caller's own (same moment, so vms - tenant_vms = other tenants)
  states: Record<string, number>
  measured_vms: number
  pss_bytes: number
  rss_bytes: number
  mem_total_bytes: number
  mem_available_bytes: number
}

// Timing is what the X-BeeVM-* headers of an /invoke answer say.
export interface Timing {
  vm: string
  start: string
  snapshot: string
  codeCache: string
  bootMs: number
  uploadMs: number
  loadMs: number
  functionMs: number
  totalMs: number
  fetchMs: number
}

export interface InvokeResult {
  status: number
  ok: boolean
  body: string
  timing: Timing
  roundTripMs: number // measured in the browser
}

export class ApiError extends Error {
  readonly status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

const num = (h: Headers, name: string) => {
  const v = Number(h.get(name))
  return Number.isFinite(v) ? v : 0
}

export function parseTiming(h: Headers): Timing {
  return {
    vm: h.get('X-BeeVM-VM-Id') ?? '',
    start: h.get('X-BeeVM-Start') ?? '',
    snapshot: h.get('X-BeeVM-Snapshot') ?? '',
    codeCache: h.get('X-BeeVM-Code-Cache') ?? '',
    bootMs: num(h, 'X-BeeVM-Boot-Ms'),
    uploadMs: num(h, 'X-BeeVM-Upload-Ms'),
    loadMs: num(h, 'X-BeeVM-Load-Ms'),
    functionMs: num(h, 'X-BeeVM-Function-Ms'),
    totalMs: num(h, 'X-BeeVM-Total-Ms'),
    fetchMs: num(h, 'X-BeeVM-Fetch-Ms'),
  }
}

// errorText pulls the "error" field out of an error answer, or returns the raw text.
export function errorText(status: number, body: string): string {
  try {
    const e = JSON.parse(body)
    if (e && typeof e.error === 'string') return e.error
  } catch {
    // not JSON
  }
  return body.trim() || `HTTP ${status}`
}

export class Client {
  readonly key: string
  readonly base: string
  constructor(key: string, base = '') {
    this.key = key
    this.base = base
  }

  headers(extra: Record<string, string> = {}): Record<string, string> {
    return { Authorization: `Bearer ${this.key}`, ...extra }
  }

  private async json<T>(method: string, path: string, signal?: AbortSignal): Promise<T> {
    const r = await fetch(this.base + path, { method, headers: this.headers(), signal })
    const text = await r.text()
    if (!r.ok) throw new ApiError(r.status, errorText(r.status, text))
    return JSON.parse(text) as T
  }

  async functions(signal?: AbortSignal): Promise<Fn[]> {
    return (await this.json<{ functions: Fn[] }>('GET', '/api/v1/functions', signal)).functions
  }
  async vms(signal?: AbortSignal): Promise<VM[]> {
    return (await this.json<{ vms: VM[] }>('GET', '/api/v1/vms', signal)).vms
  }
  node(signal?: AbortSignal): Promise<NodeInfo> {
    return this.json<NodeInfo>('GET', '/api/v1/node', signal)
  }
  prewarm(name: string, count: number): Promise<{ requested: number; started: number }> {
    return this.json('POST', `/api/v1/functions/${encodeURIComponent(name)}/prewarm?count=${count}`)
  }

  // invoke calls a function. Function failures (502) are results, not exceptions: the timing
  // headers are still there. Other errors throw ApiError.
  async invoke(name: string, payload: string): Promise<InvokeResult> {
    const t0 = performance.now()
    const r = await fetch(`${this.base}/invoke/${encodeURIComponent(name)}`, {
      method: 'POST',
      headers: this.headers({ 'Content-Type': 'application/json' }),
      body: payload,
    })
    const body = await r.text()
    const roundTripMs = performance.now() - t0
    if (!r.ok && r.status !== 502) throw new ApiError(r.status, errorText(r.status, body))
    return { status: r.status, ok: r.ok, body, timing: parseTiming(r.headers), roundTripMs }
  }
}
