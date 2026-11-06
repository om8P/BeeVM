// Server-Sent Events over fetch.
//
// The browser's built-in EventSource cannot send an Authorization header, and putting the API
// key in the URL would leak it into logs and history. So we read the stream ourselves: fetch
// gives the body as a stream of bytes, and SSE is a simple text format on top:
//
//   event: vm
//   data: {"id":"vm-3","state":"busy",...}
//   <empty line = end of one message>
//
// Lines starting with ":" are comments (the server's keep-alive pings).

export interface SSEMessage {
  event: string
  data: string
}

// SSEParser turns text chunks (cut anywhere, even in the middle of a line) into messages.
export class SSEParser {
  private buf = ''
  private event = ''
  private data: string[] = []

  push(chunk: string): SSEMessage[] {
    this.buf += chunk
    const out: SSEMessage[] = []
    let nl: number
    while ((nl = this.buf.indexOf('\n')) >= 0) {
      let line = this.buf.slice(0, nl)
      this.buf = this.buf.slice(nl + 1)
      if (line.endsWith('\r')) line = line.slice(0, -1)

      if (line === '') {
        // End of a message.
        if (this.data.length > 0 || this.event !== '') {
          out.push({ event: this.event || 'message', data: this.data.join('\n') })
        }
        this.event = ''
        this.data = []
        continue
      }
      if (line.startsWith(':')) continue // comment
      const colon = line.indexOf(':')
      const field = colon < 0 ? line : line.slice(0, colon)
      let value = colon < 0 ? '' : line.slice(colon + 1)
      if (value.startsWith(' ')) value = value.slice(1)
      if (field === 'event') this.event = value
      else if (field === 'data') this.data.push(value)
      // id and retry are not used by beevmd
    }
    return out
  }
}

export type StreamStatus = 'connecting' | 'live' | 'unauthorized'

export interface StreamOptions {
  url: string
  headers: Record<string, string>
  onMessage: (m: SSEMessage) => void
  onStatus: (s: StreamStatus) => void
}

// connectEvents keeps an event stream open: it reconnects after errors, waiting a little longer
// each time (0.5 s, 1 s, 2 s ... at most 10 s). It stops for good on 401. Call the returned
// function to close it.
export function connectEvents(opts: StreamOptions): () => void {
  const ctrl = new AbortController()
  let delay = 500

  const run = async () => {
    while (!ctrl.signal.aborted) {
      opts.onStatus('connecting')
      try {
        const r = await fetch(opts.url, {
          headers: { ...opts.headers, Accept: 'text/event-stream' },
          signal: ctrl.signal,
          cache: 'no-store',
        })
        if (r.status === 401) {
          opts.onStatus('unauthorized')
          return
        }
        if (!r.ok || !r.body) throw new Error(`HTTP ${r.status}`)
        const reader = r.body.pipeThrough(new TextDecoderStream()).getReader()
        const parser = new SSEParser()
        let first = true
        for (;;) {
          const { value, done } = await reader.read()
          if (done) break
          for (const m of parser.push(value)) {
            if (first) {
              opts.onStatus('live')
              delay = 500
              first = false
            }
            opts.onMessage(m)
          }
        }
      } catch {
        if (ctrl.signal.aborted) return
      }
      // The stream ended (server restarted) or failed: try again shortly.
      opts.onStatus('connecting')
      await sleep(delay, ctrl.signal)
      delay = Math.min(delay * 2, 10_000)
    }
  }
  void run()
  return () => ctrl.abort()
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms)
    signal.addEventListener('abort', () => {
      clearTimeout(t)
      resolve()
    })
  })
}
