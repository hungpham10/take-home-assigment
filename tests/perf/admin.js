// k6 perf — admin slice: introspect 50–100 QPS peak (plans/14).
// Chạy: k6 run -e BASE=... -e TOKEN=abt_... -e TENANT=<uuid> tests/perf/admin.js
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE || 'http://localhost:8080';
export const options = {
  scenarios: {
    introspect: {
      executor: 'constant-arrival-rate',
      rate: 80,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  thresholds: {
    'http_req_duration{scenario:introspect}': ['p(95)<100'],
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  const res = http.post(`${BASE}/api/admin/v1/token/introspect`,
    JSON.stringify({ token: __ENV.TOKEN || 'abt_invalid' }),
    { headers: { 'Content-Type': 'application/json', 'X-Tenant-Id': __ENV.TENANT || '' } });
  check(res, { 'introspect 200': (r) => r.status === 200 });
}
