// Produces the expected numbers for internal/power tests by running upstream device-monitor's
// own code (pinned at c3a4c41) over cases.json. Run from the repo root:
//
//   node internal/power/testdata/upstream.js <device-monitor checkout> > internal/power/testdata/expected.json
//
// estimate and fromSample come from power.js; PRICE_REGIONS and TZ_REGIONS are evaluated
// straight out of public/index.html.
const fs = require('fs');
const path = require('path');

const root = process.argv[2];
if (!root) { console.error('usage: node upstream.js <device-monitor dir>'); process.exit(2); }
const power = require(path.resolve(root, 'power.js'));
const cases = JSON.parse(fs.readFileSync(path.join(__dirname, 'cases.json'), 'utf8'));

// Pull a `const NAME = { ... };` literal out of the dashboard source and evaluate it.
const html = fs.readFileSync(path.resolve(root, 'public/index.html'), 'utf8');
function table(name) {
  const m = html.match(new RegExp(`const ${name} = (\\{[\\s\\S]*?\\n\\});`));
  if (!m) throw new Error(`${name} not found in index.html`);
  return Function(`return ${m[1]}`)();
}

const out = {
  estimate: cases.estimate.map(c => ({ name: c.name, want: power.estimate(c.report) })),
  fromSample: cases.fromSample.map(c => ({ name: c.name, want: power.fromSample(c.sample, c.hw) })),
  priceRegions: Object.entries(table('PRICE_REGIONS')).map(([code, [name, currency, rate]]) => ({ code, name, currency, rate })),
  tzRegions: table('TZ_REGIONS'),
};
process.stdout.write(JSON.stringify(out, null, 1) + '\n');
