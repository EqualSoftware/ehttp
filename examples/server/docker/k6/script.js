// A short load test through nginx. Run: docker compose --profile load run --rm k6
// K6_VUS and K6_DURATION set the size (defaults: 5 users, 20 s).
import http from 'k6/http';
import encoding from 'k6/encoding';
import { check, sleep } from 'k6';

const base = __ENV.BASE_URL || 'http://nginx:8080';
// Demo credentials of the /admin group (examples/server/app/app.go).
const basic = 'Basic ' + encoding.b64encode('demo:demo-password');

export const options = {
  vus: Number(__ENV.K6_VUS || 5),
  duration: __ENV.K6_DURATION || '20s',
  thresholds: {
    checks: ['rate>0.99'],
    http_req_duration: ['p(95)<500'],
  },
};

export function setup() {
  const res = http.get(`${base}/`, { headers: { Accept: 'application/json' } });
  return { etag: res.headers['Etag'] };
}

export default function (data) {
  const json = { headers: { Accept: 'application/json' } };

  check(http.get(`${base}/`, json), { 'json 200': (r) => r.status === 200 });
  check(http.get(`${base}/`, { headers: { Accept: 'application/json', 'If-None-Match': data.etag } }),
    { '304': (r) => r.status === 304 });
  check(http.get(`${base}/file`, { headers: { Range: 'bytes=0-9' } }),
    { 'range 206': (r) => r.status === 206 });
  check(http.get(`${base}/admin/me`), { 'no credentials 401': (r) => r.status === 401 });
  check(http.get(`${base}/admin/me`, { headers: { Authorization: basic } }),
    { 'credentials 200': (r) => r.status === 200 });
  check(http.get(`${base}/api/v1/hello/k6`), { 'param 200': (r) => r.status === 200 });
  check(http.get(`${base}/flaky`), { 'flaky 200': (r) => r.status === 200 });
  sleep(0.1);
}
