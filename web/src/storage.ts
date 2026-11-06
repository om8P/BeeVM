// The API key lives in sessionStorage (gone when the tab closes), or in localStorage when the
// user ticks "remember". Storage can be blocked (private windows, strict settings), so every
// access is wrapped: the dashboard still works, it just asks for the key again.

const KEY = 'beevm.apiKey'

export function loadKey(): string {
  for (const s of [safe(() => sessionStorage), safe(() => localStorage)]) {
    const v = s && safe(() => s.getItem(KEY))
    if (v) return v
  }
  return ''
}

export function saveKey(key: string, remember: boolean) {
  const s = safe(() => (remember ? localStorage : sessionStorage))
  if (s) safe(() => s.setItem(KEY, key))
}

export function forgetKey() {
  for (const s of [safe(() => sessionStorage), safe(() => localStorage)]) {
    if (s) safe(() => s.removeItem(KEY))
  }
}

function safe<T>(f: () => T): T | undefined {
  try {
    return f()
  } catch {
    return undefined
  }
}
