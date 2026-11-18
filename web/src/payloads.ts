// Payloads for the Invoke panel. The server does not know what input a function expects, so
// the dashboard (1) remembers the last payload typed for each function, in this browser, and
// (2) for a function never called from here, starts from a sample that fits the example
// functions in examples/functions (matched by name), or {} for anything else.

// SAMPLES: the first rule whose pattern matches the function's name wins. Keep these harmless:
// forkbomb only counts processes, netprobe only prints the VM's addresses.
const SAMPLES: [RegExp, unknown][] = [
  [/hello/i, { name: 'BeeVM' }],
  [/cpu/i, { threads: 1, ms: 500 }],
  [/mem/i, { mib: 64 }],
  [/fork/i, { mode: 'count' }],
  [/net/i, { mode: 'info' }],
]

// samplePayload is the starting payload for a function never called from this browser.
export function samplePayload(fnName: string): string {
  for (const [re, payload] of SAMPLES) {
    if (re.test(fnName)) return JSON.stringify(payload)
  }
  return '{}'
}

const KEY = 'beevm.payload.'

// Storage can be missing or throw (private window, blocked site data): then nothing is kept.
export function loadPayload(fnName: string, store: Storage | undefined = safeLocalStorage()): string {
  try {
    const saved = store?.getItem(KEY + fnName)
    if (saved !== null && saved !== undefined) return saved
  } catch {
    // fall through to the sample
  }
  return samplePayload(fnName)
}

export function savePayload(fnName: string, payload: string, store: Storage | undefined = safeLocalStorage()): void {
  try {
    store?.setItem(KEY + fnName, payload)
  } catch {
    // not kept; the panel still works
  }
}

function safeLocalStorage(): Storage | undefined {
  try {
    return typeof localStorage === 'undefined' ? undefined : localStorage
  } catch {
    return undefined
  }
}
