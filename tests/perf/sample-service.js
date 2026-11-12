// k6 script for examples/sample-service (plans/43).
//   k6 run tests/perf/sample-service.js
//   SUT=http://localhost:8095 VUS=20 DURATION=30s k6 run tests/perf/sample-service.js
//
// Thresholds are deliberately loose: this is a demo/fixture, not a benchmark.
// The failing-threshold fixture lives in sample-service-fail.js.
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.SUT || 'http://localhost:8095';

export const options = {
  vus: Number(__ENV.VUS || 5),
  duration: __ENV.DURATION || '10s',
  thresholds: {
    http_req_duration: ['p(95)<500'],
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
  },
};

export default function () {
  const list = http.get(`${BASE}/todos`);
  check(list, { 'list 200': (r) => r.status === 200 });

  const created = http.post(`${BASE}/todos`, JSON.stringify({ text: 'from k6' }), {
    headers: { 'Content-Type': 'application/json' },
  });
  check(created, { 'create 201': (r) => r.status === 201 });
}
