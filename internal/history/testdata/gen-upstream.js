// Records upstream device-monitor history.js (pinned c3a4c41) running a fixed scenario, so the
// Go port can be checked against it. Run from the repo root:
//
//   node internal/history/testdata/gen-upstream.js <path-to-device-monitor-checkout>
//
// It writes testdata/upstream/ops.ndjson (every call made and, for queries, upstream's answer)
// and testdata/upstream/history/*.ndjson (the files upstream left on disk). Date.now is faked,
// so the output is deterministic.
const fs = require('fs');
const os = require('os');
const path = require('path');

const src = process.argv[2];
if (!src) { console.error('usage: gen-upstream.js <device-monitor dir>'); process.exit(2); }
const { HistoryStore } = require(path.resolve(src, 'history.js'));

let now = 0;
Date.now = () => now;

const out = path.join(__dirname, 'upstream');
const data = fs.mkdtempSync(path.join(os.tmpdir(), 'mm-history-'));
const ops = [];
const log = op => ops.push(JSON.stringify(op));

// Deterministic PRNG (mulberry32) so values look like real metrics but never change.
let seed = 7;
const rnd = () => { seed |= 0; seed = seed + 0x6D2B79F5 | 0; let t = Math.imul(seed ^ seed >>> 15, 1 | seed); t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t; return ((t ^ t >>> 14) >>> 0) / 4294967296; };
const r = (lo, hi, nullChance = 0) => rnd() < nullChance ? null : +(lo + rnd() * (hi - lo)).toFixed(3);
const sample = t => ({
  t, cpu: r(0, 100), gpu: r(0, 100, 0.2), mem: r(10, 90), cpuTemp: r(35, 95, 0.1), gpuTemp: r(30, 85, 0.2),
  gpuPower: r(5, 350, 0.3), power: r(20, 600, 0.1), rx: r(0, 1e7), tx: r(0, 1e6), dr: r(0, 5e8, 0.05), dw: r(0, 5e8, 0.05),
});

let store;
const open = () => { log({ op: 'open', now }); store = new HistoryStore(data); };
const close = () => { log({ op: 'close', now }); store.close(); };
const add = (id, s) => { log({ op: 'add', now, id, sample: s }); store.add(id, s); };
const query = (id, from, to, points) => { log({ op: 'query', now, id, from, to, points, want: store.query(id, from, to, points) }); };
const minutes = (id, from, to) => { log({ op: 'minutePoints', now, id, from, to, want: store.minutePoints(id, from, to) }); };
const recent = (id, ms) => { log({ op: 'recent', now, id, ms, want: store.recent(id, ms) }); };
const remove = id => { log({ op: 'remove', now, id }); store.remove(id); };

const MIN = 60e3, DAY = 86400e3;
const T0 = Date.UTC(2026, 9, 1, 0, 0, 0);   // 2026-10-01T00:00:00Z
const A = 'machine-a', B = 'host b/weird:id';   // B exercises safe() file names

// Seed 31 days of sparse minute history for A by running the store at old times, so the
// reload and commit prunes have something to drop.
now = T0; open();
for (let d = 0; d < 31; d++) {
  for (let m = 0; m < 3; m++) {
    const base = T0 + d * DAY + m * 7 * MIN;
    for (let k = 0; k < 4; k++) { now = base + k * 2000 + 13; add(A, sample(now)); }
  }
}
now = T0 + 31 * DAY; close();

// Restart mid-minute: the second process sees the same minute again (v1.2.29 fix).
const live = T0 + 31 * DAY + 5 * MIN;   // 2026-11-01T00:05Z
now = live; open();
for (let i = 0; i < 20; i++) { now = live + i * 2000 + 500; add(A, sample(now)); add(B, sample(now)); }
now += 100; close();     // flushes the half-finished minute 00:05 and 00:06
now += 900; open();      // restart in the same minute
for (let i = 0; i < 200; i++) { now += 2000; add(A, sample(now)); if (i % 3) add(B, sample(now)); }
// A sample whose average is an exact rounding tie (0.125) for toFixed(2).
now = Math.floor(now / MIN) * MIN + MIN + 1; add(A, { t: now, cpu: 0.125, gpu: null, mem: 2.375, cpuTemp: null, gpuTemp: null, gpuPower: null, power: 1.005, rx: 0, tx: 0, dr: null, dw: null });
now += MIN; add(A, sample(now));

const end = now;
for (const [range, pts] of [[10 * MIN, 1200], [60 * MIN, 1200], [60 * MIN, 300], [6 * 60 * MIN, 1200], [DAY, 1200], [7 * DAY, 1200], [30 * DAY, 1200], [30 * DAY, 77.5], [45 * MIN, 1000]]) {
  query(A, end - range, end, pts); query(B, end - range, end, pts);
}
query(A, end - 2 * DAY + 12345.6, end - DAY / 3, 333);
query('nobody', end - DAY, end, 1200);
minutes(A, end - 3 * 60 * MIN, end); minutes(A, T0, end);
recent(A, 10 * MIN); recent(B, 90 * MIN);
close();

// Reopen: load() reads the files written above, merges nothing new, and serves queries.
now += 5000; open();
query(A, end - 7 * DAY, end, 1200); query(B, end - DAY, end, 500); query(A, end - 10 * MIN, end, 1200);
// Prune on commit: a day later the oldest rows fall out and the file is rewritten.
now = end + 2 * DAY; add(A, sample(now)); now += MIN; add(A, sample(now));
query(A, now - 30 * DAY, now, 1200);
remove('nobody');
close();

fs.mkdirSync(path.join(out, 'history'), { recursive: true });
for (const f of fs.readdirSync(path.join(out, 'history'))) fs.unlinkSync(path.join(out, 'history', f));
for (const f of fs.readdirSync(path.join(data, 'history'))) fs.copyFileSync(path.join(data, 'history', f), path.join(out, 'history', f));
fs.writeFileSync(path.join(out, 'ops.ndjson'), ops.join('\n') + '\n');
fs.rmSync(data, { recursive: true, force: true });
console.log(`wrote ${ops.length} ops, ${fs.readdirSync(path.join(out, 'history')).length} history files`);
