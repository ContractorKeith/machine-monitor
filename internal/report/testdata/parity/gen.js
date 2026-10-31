// Regenerates cases.json: runs upstream security.js (pinned c3a4c41) over the M0 fixtures,
// hand-picked edge cases and seeded random reports, recording what upstream produces.
// TestParity checks Normalize against it. Not run by go test; needs node:
//   node internal/report/testdata/parity/gen.js /path/to/device-monitor > internal/report/testdata/parity/cases.json
const fs = require('fs');
const path = require('path');
const sec = require(path.resolve(process.argv[2], 'security.js'));

// Mirrors server.js handleReport: id check, then normalizeReport and normalizeProcesses.
function run(body) {
  let raw;
  try { raw = JSON.parse(body); } catch { return { error: 'bad json' }; }
  try {
    if (!sec.isValidId(raw.id)) return { error: 'invalid id' };
  } catch { return { error: 'invalid id' }; }   // null body: raw.id throws
  return { report: sec.normalizeReport(raw), processes: sec.normalizeProcesses(raw.processes) };
}

const cases = [];
const add = (name, body) => cases.push({ name, body: typeof body === 'string' ? body : JSON.stringify(body), want: null });

for (const f of fs.readdirSync(path.join(__dirname, '..')).filter(f => f.endsWith('.json')).sort()) {
  add('fixture ' + f, fs.readFileSync(path.join(__dirname, '..', f), 'utf8'));
}

const ID = 'abc123';
add('null body', 'null');
add('array body', '[1,2]');
add('string body', '"x"');
add('missing id', {});
add('numeric id', { id: 12345 });
add('id 64 chars', { id: 'a'.repeat(64) });
add('id 65 chars', { id: 'a'.repeat(65) });
add('id leading dot', { id: '.abc' });
add('id trailing newline', { id: 'abc\n' });
add('id with colon', { id: 'A1:b.c-d_e' });
add('empty object', { id: ID });
add('overflow numbers', `{"id":"${ID}","uptime":1e999,"cpu":{"usage":-1e999,"temp":1e-999},"memory":{"total":1.7976931348623157e308}}`);
add('duplicate keys', `{"id":"${ID}","hostname":"a","hostname":"b"}`);
add('numeric strings', { id: ID, uptime: ' 42 ', cpu: { usage: '0x1A', threads: '0b101', clockMHz: '0o17', temp: '-1e1', cores: ['1.5', '.5', '5.', '+3', '-0', '1_0', '0x', '-0x1', 'Infinity', '1e5', ' ', '', 'abc', '\u00a012\ufeff', '\u008512'] } });
add('numeric string edge', { id: ID, memory: { total: '0x' + 'f'.repeat(300), used: '1e400' }, net: { rx: '0X1f', tx: '0B11' }, diskIO: { read: '1e-400', write: '00012' } });
add('non-number types', { id: ID, uptime: true, cpu: { usage: [5], threads: { a: 1 }, clockMHz: null, temp: false } });
add('clamps', { id: ID, uptime: 2e10, power: { cpu: 9999 }, cpu: { usage: 150, temp: -100, threads: 1e6, clockMHz: -5, cores: [-1, 101, 50] }, memory: { total: 1e19, used: -1 } });
add('negative zero', `{"id":"${ID}","cpu":{"temp":-0,"usage":-0},"temps":[{"name":"x","value":-0.0}]}`);
add('string coercion', { id: ID, hostname: 12345, os: true, cpu: { name: [1, [2, null], 'a', { b: 1 }] }, gpus: [{ name: { a: 1 }, vendor: 1e21 }, { name: 1e-7, vendor: 0.000001 }, { name: 123.456, vendor: -0 }, { name: 1.5e300, vendor: 2 ** 53 + 2 }] });
add('control chars and trim', { id: ID, hostname: '\u0000\u001f host\u007f name \t\n', os: '\u00a0\u2003 Ubuntu \u3000\ufeff', cpu: { name: '\u0085 keep NEL \u0085' } });
add('empty strings', { id: ID, hostname: '', os: '   ', cpu: { name: '\u0001\u0002' } });
add('string limits', { id: ID, hostname: 'h'.repeat(100), os: 'o'.repeat(200), cpu: { name: 'é'.repeat(130) }, actionResult: { id: 'abcdef012345', ok: 'true', message: 'm'.repeat(300) } });
add('cut leaves trailing space', { id: ID, hostname: 'a'.repeat(63) + ' b' });
add('platform enum', { id: ID, platform: 'Linux' });
add('platform macos', { id: ID, platform: 'macos', formFactor: 'laptop' });
add('platform windows', { id: ID, platform: 'windows', formFactor: 'Desktop' });
add('platform darwin', { id: ID, platform: 'darwin', formFactor: 'desktop' });
add('power not object', { id: ID, power: [1] });
add('power null cpu', { id: ID, power: {} });
add('features and actions', { id: ID, features: ['procs', 'x', 'actions', 'procs', 1, null, 'a', 'b', 'actions', 'procs'], actions: ['sleep', 'reboot', 'restart', 'shutdown', 'SLEEP', 'sleep', 'sleep', 'sleep', 'sleep', 'shutdown'] });
add('features not array', { id: ID, features: 'procs', actions: { 0: 'sleep' } });
add('macs', { id: ID, macs: ['AA:BB:CC:DD:EE:FF', 'aa-bb-cc-dd-ee-01', 'aa:bb-cc:dd-ee:02', '00:00:00:00:00:00', '00-00-00-00-00-00', '00:00-00:00-00:00', 'aa:bb:cc:dd:ee', 'gg:bb:cc:dd:ee:ff', 7, 'aabbccddeeff', ' aa:bb:cc:dd:ee:ff'] });
add('macs over 8', { id: ID, macs: Array.from({ length: 10 }, (_, i) => `aa:bb:cc:dd:ee:0${i}`) });
add('action result ok', { id: ID, actionResult: { id: '0a1b2c3d4e5f', ok: true, message: ' done ' } });
add('action result upper id', { id: ID, actionResult: { id: '0A1B2C3D4E5F', ok: true } });
add('action result short id', { id: ID, actionResult: { id: '0a1b2c3d4e5', ok: true } });
add('action result ok 1', { id: ID, actionResult: { id: '0a1b2c3d4e5f', ok: 1, message: 404 } });
add('action result array', { id: ID, actionResult: ['0a1b2c3d4e5f'] });
for (const v of ['1.2.29', '1.2', '12345.1.1', '1.1.1234567', 'v1.2.3', '1.2.3-beta', '1.2.3\n', '0.0.0', '9999.9999.999999', '١.٢.٣']) add('appVersion ' + v, { id: ID, appVersion: v });
add('appVersion number', { id: ID, appVersion: 1.2 });
add('tempSource', { id: ID, cpu: { tempSource: 'ACPI' } });
add('tempSource acpi', { id: ID, cpu: { tempSource: 'acpi' } });
add('cores over 1024', { id: ID, cpu: { cores: Array.from({ length: 1030 }, (_, i) => i % 7 ? i % 101 : null) } });
add('cpu not object', { id: ID, cpu: [1, 2], memory: 'lots' });
add('gpus mixed', { id: ID, gpus: [null, 1, 'x', [], { name: 'RTX', vendor: 'nvidia-corporation-long-name', usage: 120, temp: 300, memUsed: -5, memTotal: '24e9', power: 2e5, powerLimit: '450', fan: 101, clockMHz: '2520' }] });
add('gpus over 32', { id: ID, gpus: Array.from({ length: 40 }, (_, i) => (i === 0 ? 'skip' : { name: 'g' + i })) });
add('disks', { id: ID, disks: [{ name: 'C:\\', label: 'L'.repeat(90), total: '1000', used: 2000 }, 'x'] });
add('disks over 64', { id: ID, disks: Array.from({ length: 70 }, (_, i) => ({ name: 'd' + i })) });
add('net and diskIO', { id: ID, net: { rx: -1, tx: 'big' }, diskIO: [] });
add('temps', { id: ID, temps: [{ name: 'Tctl', parent: '/amdcpu/0', value: 251 }, { value: -61 }, {}] });
add('temps over 256', { id: ID, temps: Array.from({ length: 260 }, (_, i) => ({ name: 't' + i, value: i })) });
add('fans', { id: ID, fans: [{ name: 'CPU', rpm: 2e5 }, { name: null, rpm: '1200' }] });
add('fans over 64', { id: ID, fans: Array.from({ length: 70 }, (_, i) => ({ name: 'f' + i, rpm: i })) });
add('ai not array', { id: ID, ai: { app: 'Ollama' } });
add('ai empty', { id: ID, ai: [] });
add('ai filters', { id: ID, ai: [{ app: '', mem: 1 }, { app: 'Ollama', mem: '5e9', models: [{ name: '' }, { name: 'llama3', size: '4e9', kind: 'k'.repeat(40) }, 'x', { size: 1 }] }, { app: 'A'.repeat(50), models: 'none' }, null] });
add('ai over 8', { id: ID, ai: Array.from({ length: 10 }, (_, i) => ({ app: 'app' + i, models: Array.from({ length: 20 }, (_, j) => ({ name: 'm' + j })) })) });
add('processes not array', { id: ID, processes: { pid: 1, name: 'x' } });
add('processes empty after filter', { id: ID, processes: [{ pid: 1 }, { name: 'x' }, { pid: 'a', name: 'y' }, null] });
add('processes clamps', { id: ID, processes: [{ pid: '2e9', name: 'n'.repeat(90), cmd: 'c'.repeat(200), user: 'u'.repeat(50), cpu: 150, mem: -1, gpu: '101', gpuMem: '1e3' }, { pid: 0, name: 'idle' }, { pid: -5, name: 'neg' }] });
add('processes over 120', { id: ID, processes: Array.from({ length: 130 }, (_, i) => ({ pid: i, name: 'p' + i })) });
add('extra keys dropped', { id: ID, token: 'secret', __proto__x: 1, constructor: 'x', receivedAt: 5, powerEst: 9 });

// Seeded random reports: values drawn from a pool of awkward types for every field.
let seed = 42;
const rnd = () => ((seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648);
const pick = a => a[Math.floor(rnd() * a.length)];
const STR = ['', ' ', 'x', ' padded ', 'tab\there', 'nul\u0000x', '\u007f', 'é', 'ü'.repeat(70), 'z'.repeat(250), '12', '0x10', '1e3', '-7', 'NaN', 'Infinity', 'true'];
const leaf = () => pick([null, true, false, 0, -0.5, 1, 99.9, 100.1, 1e3, 1e5 + 1, -1e3, 2 ** 31, 1e21, 1e-7, 5e18, ...STR, [], [1, 'a'], {}, { a: 1 }]);
const many = (n, f) => (rnd() < 0.15 ? leaf() : Array.from({ length: Math.floor(rnd() * n) }, f));
const objOr = f => (rnd() < 0.15 ? leaf() : f());
const keys = (ks) => Object.fromEntries(ks.filter(() => rnd() < 0.8).map(k => [k, leaf()]));
for (let i = 0; i < 120; i++) {
  add('random ' + i, {
    id: rnd() < 0.05 ? leaf() : 'dev-' + i,
    ...keys(['hostname', 'os', 'uptime', 'appVersion', 'formFactor', 'platform']),
    platform: pick(['linux', 'macos', 'windows', 'Linux', 1, null]),
    formFactor: pick(['laptop', 'desktop', 'tablet', null]),
    power: objOr(() => keys(['cpu'])),
    features: many(10, () => pick(['procs', 'actions', 'x', 1])),
    actions: many(10, () => pick(['sleep', 'restart', 'shutdown', 'hibernate', null])),
    macs: many(10, () => pick(['aa:bb:cc:dd:ee:ff', 'AA-BB-CC-DD-EE-FF', '00:00:00:00:00:00', 'bad', 3])),
    actionResult: objOr(() => ({ id: pick(['0a1b2c3d4e5f', 'ABCDEF012345', 'x', 1]), ok: leaf(), message: leaf() })),
    cpu: objOr(() => ({ ...keys(['name', 'usage', 'threads', 'clockMHz', 'temp', 'tempSource']), cores: many(20, leaf) })),
    memory: objOr(() => keys(['total', 'used'])),
    gpus: many(5, () => objOr(() => keys(['name', 'vendor', 'usage', 'temp', 'memUsed', 'memTotal', 'power', 'powerLimit', 'fan', 'clockMHz']))),
    disks: many(5, () => objOr(() => keys(['name', 'label', 'total', 'used']))),
    net: objOr(() => keys(['rx', 'tx'])),
    diskIO: objOr(() => keys(['read', 'write'])),
    temps: many(5, () => objOr(() => keys(['name', 'parent', 'value']))),
    fans: many(5, () => objOr(() => keys(['name', 'rpm']))),
    ai: many(4, () => objOr(() => ({ ...keys(['app', 'mem']), models: many(4, () => objOr(() => keys(['name', 'size', 'kind']))) }))),
    processes: many(5, () => objOr(() => keys(['pid', 'name', 'cmd', 'user', 'cpu', 'mem', 'gpu', 'gpuMem']))),
  });
}

for (const c of cases) c.want = run(c.body);
process.stdout.write('[\n' + cases.map(c => JSON.stringify(c)).join(',\n') + '\n]\n');
