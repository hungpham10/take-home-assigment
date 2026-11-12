// Passing/failing threshold fixture for run-result collection (plans/45).
//
// /flaky?rate=1 always returns 500, so `http_req_failed<0.01` is guaranteed to
// fail and k6 prints a ✗ line in its THRESHOLDS block — the exact marker the
// summary parser must detect. Use it to prove `thresholds_failed=true` in
// tbl_load_tests and to render a failing `ltp run report`.
import http from 'k6/http';

const BASE = __ENV.SUT || 'http://localhost:8095';

export const options = {
  vus: Number(__ENV.VUS || 3),
  duration: __ENV.DURATION || '10s',
  thresholds: {
    http_req_failed: ['rate<0.01'], // must fail: every request is a 500
  },
};

export default function () {
  http.get(`${BASE}/flaky?rate=1`);
}
