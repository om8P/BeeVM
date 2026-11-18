// Closed-loop throughput: N virtual users call warm functions back to back, N steps up
// 1, 2, 4, 8, 16, 32. Shows how many calls per second one node answers and how latency
// grows with concurrency. Deploy and call the functions once first so VMs are warm.
//
//   BEEVM_API_KEY=... k6 run -e FNS=hello bench/k6/warm.js --out csv=warm.csv
import { checkConfig, call, FNS, stage } from './common.js';

const levels = [1, 2, 4, 8, 16, 32];

export const options = {
  scenarios: {
    warm: {
      executor: 'ramping-vus',
      startVUs: 1,
      // Hold each level: jump to it in 1 s, then stay.
      stages: levels.flatMap((n) => [{ duration: '1s', target: n }, stage(20, n)]),
      gracefulRampDown: '2s',
    },
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  checkConfig();
  // Make sure every function has a VM before the measurement starts.
  for (const fn of FNS) call(fn);
}

export default function () {
  call(FNS[(__VU + __ITER) % FNS.length]);
}

export function handleSummary(data) {
  return { 'k6-warm-summary.json': JSON.stringify(data, null, 2) };
}
