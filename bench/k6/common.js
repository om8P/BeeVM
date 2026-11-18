// Shared helpers for the BeeVM k6 scripts. Environment:
//   BEEVM_URL      server address (default http://127.0.0.1:8080)
//   BEEVM_API_KEY  a tenant's key (required)
//   FNS            comma-separated deployed functions (default hello)
//   PAYLOAD        JSON body of every call (default {"name":"k6"})
//   QUICK          1 = short stages (smoke test of the scripts)
import http from 'k6/http';
import { Trend, Counter } from 'k6/metrics';

export const BASE = __ENV.BEEVM_URL || 'http://127.0.0.1:8080';
export const KEY = __ENV.BEEVM_API_KEY || '';
export const FNS = (__ENV.FNS || 'hello').split(',').map((s) => s.trim()).filter(Boolean);
export const PAYLOAD = __ENV.PAYLOAD || '{"name":"k6"}';
export const QUICK = __ENV.QUICK === '1';

// One trend per kind of start (X-BeeVM-Start header): warm, restore, boot.
const byStart = {
  warm: new Trend('beevm_call_ms_warm', true),
  restore: new Trend('beevm_call_ms_restore', true),
  boot: new Trend('beevm_call_ms_boot', true),
};
export const failed = new Counter('beevm_calls_failed');

export function checkConfig() {
  if (!KEY) {
    throw new Error('set BEEVM_API_KEY (a tenant key whose functions are deployed)');
  }
}

// call invokes one function and records which kind of start served it.
export function call(fn) {
  const res = http.post(`${BASE}/invoke/${fn}`, PAYLOAD, {
    headers: { Authorization: `Bearer ${KEY}`, 'Content-Type': 'application/json' },
    tags: { fn },
  });
  const start = res.headers['X-Beevm-Start'] || 'none';
  if (res.status !== 200) {
    failed.add(1);
    return res;
  }
  if (byStart[start]) {
    byStart[start].add(res.timings.duration);
  }
  return res;
}

export function stage(seconds, target) {
  return { duration: `${QUICK ? Math.max(2, Math.round(seconds / 4)) : seconds}s`, target };
}
