// k6 perf — control plane (plans/14 bảng QPS target).
// Chạy: k6 run -e BASE=http://localhost:8080 -e TENANT=<uuid> -e USER=<uuid> tests/perf/control-plane.js
import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE = __ENV.BASE || 'http://localhost:8080';
const H = {
  'X-Tenant-Id': __ENV.TENANT || '',
  'X-User-Id': __ENV.USER || '',
  'Content-Type': 'application/json',
};

// Bursty control-plane profile: reads chiếm đa số, writes thưa (plans/14).
export const options = {
  scenarios: {
    reads: {
      executor: 'constant-arrival-rate',
      rate: 40, // namespaces reads target 30–50 QPS
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  thresholds: {
    // p95 < 100ms reads (plans/14)
    'http_req_duration{scenario:reads}': ['p(95)<100'],
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  const res = http.get(`${BASE}/api/loadtest/v1/namespaces`, { headers: H });
  check(res, { 'list 200': (r) => r.status === 200 });
  http.get(`${BASE}/health`);
  sleep(0.02);
}
