// Open-loop mixed traffic: a fixed arrival rate round-robin over the functions in FNS (hello,
// hello-python, hello-node), like real users: calls arrive whether or not earlier ones finished.
// A server that cannot keep up shows dropped iterations instead of a lower rate.
//
//   BEEVM_API_KEY=... k6 run -e FNS=hello,hello-python,hello-node -e RATE=50 bench/k6/mixed.js
import { checkConfig, call, FNS, QUICK } from './common.js';

const rate = parseInt(__ENV.RATE || '50', 10);

export const options = {
  scenarios: {
    mixed: {
      executor: 'constant-arrival-rate',
      rate,
      timeUnit: '1s',
      duration: QUICK ? '8s' : '60s',
      preAllocatedVUs: 20,
      maxVUs: 200,
    },
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  checkConfig();
  for (const fn of FNS) call(fn);
}

export default function () {
  call(FNS[__ITER % FNS.length]);
}

export function handleSummary(data) {
  return { 'k6-mixed-summary.json': JSON.stringify(data, null, 2) };
}
