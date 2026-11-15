const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const html = fs.readFileSync(process.argv[2], 'utf8');
const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
assert.equal(scripts.length, 1, 'dashboard must have one inline script');
const elements = new Map();
let document;
const element = id => {
  if (!elements.has(id)) {
    const node = {id, value: '', checked: false, textContent: '', options: [], style: {}, listeners: {}, children: [],
      addEventListener(type, fn) { this.listeners[type] = fn; },
      contains(other) { return this === other || this.children.some(child => child.contains(other)); },
      focus(options) { document.activeElement = this; this.focusOptions = options; },
    };
    let body = '';
    Object.defineProperty(node, 'innerHTML', {
      get() { return body; },
      set(value) {
        // A real DOM replacement removes descendants and loses their focus.
        if (node.children.some(child => child.contains(document.activeElement))) document.activeElement = null;
        for (const child of node.children) elements.delete(child.id);
        body = value;
        node.children = [...value.matchAll(/\bid="([^"]+)"/g)].map(match => element(match[1]));
      },
    });
    elements.set(id, node);
  }
  return elements.get(id);
};
document = {activeElement: null, getElementById: id => elements.get(id) || null};
for (const match of html.split('<script>')[0].matchAll(/\bid="([^"]+)"/g)) element(match[1]);
element('memory').value = '10G';
element('cpus').value = '6';
element('distro').value = 'kubeadm';
element('gpu-driver').value = 'device-plugin';
element('cni').value = 'kindnet';
const requests = [];
const meta = {
  cnis: ['kindnet'], distros: ['kubeadm'], gpuImages: ['fedora-44'], gpuDrivers: ['device-plugin', 'dra'],
  capabilities: ['container', 'krunkit'].map(backend => ({
    backend, distro: 'kubeadm', versions: ['1.37.0'], defaultVersion: '1.37.0',
    ...(backend === 'krunkit' ? {gpuDrivers: {'device-plugin': ['1.37.0'], dra: ['1.37.0']}} : {}),
  })),
};
let handler = url => {
  if (url === '/api/meta') return meta;
  if (url === '/api/clusters') return [];
  throw new Error('unexpected bootstrap request: ' + url);
};
// Run the complete shipped script, including startup. Timers are inert so
// tests explicitly control polls; browser DOM operations use small test doubles.
const context = vm.createContext({document, fetch: async (url, opts) => { requests.push(url); return {json: async () => handler(url, opts)}; }, Date, AbortSignal, setInterval: () => 0, clearInterval: () => {}});
vm.runInContext(scripts[0][1], context);
const run = code => vm.runInContext(code, context);
const gpu = {name: 'gpu', nodes: [{name: 'kiac-gpu-gpu-1', role: 'gpu-worker', status: 'running'}]};
const ordinary = {name: 'dev', nodes: [{name: 'kiac-dev-worker-1', role: 'worker', status: 'running'}]};
const now = () => new Date().toISOString();
const snapshot = () => ({state: 'ready', capturedAt: now(), inspection: {status: {driver: 'dra', nodes: [{name: 'kiac-gpu-gpu-1', health: 'healthy', healthScope: 'vulkan-transfer', driverReady: true, renderDevice: true, schedulable: true, memoryMiB: 4096}]}, devices: [{node: 'kiac-gpu-gpu-1', device: 'apple-gpu', capacity: '4Gi', reserved: '1Gi', unreserved: '3Gi'}], claims: [{namespace: 'demo', name: 'claim', state: 'allocated', allocations: [{pool: 'node', device: 'apple-gpu', memory: '1Gi'}], consumers: ['pod']}], workloads: [{namespace: 'demo', name: 'pod', phase: 'Pending', reason: 'Unschedulable', message: '<script>bad</script>', claims: ['claim']}], notes: []}});

(async () => {
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(requests.sort(), ['/api/clusters', '/api/meta'], 'startup must not probe GPU APIs without GPU clusters');
  assert.equal(element('memory').value, '10G', 'metadata loading must preserve custom memory');
  assert.equal(element('cpus').value, '6', 'metadata loading must preserve custom CPU count');
  const hardwareButton = document.getElementById('inspect-host-gpu');
  hardwareButton.focus();
  run('renderHostGPU()');
  assert.notEqual(document.activeElement, hardwareButton, 'hardware button must be a replacement DOM node');
  assert.equal(document.activeElement, document.getElementById('inspect-host-gpu'), 'hardware focus must survive redraw');
  assert.equal(document.activeElement.focusOptions.preventScroll, true);
  requests.length = 0;
  handler = () => ({state: 'collecting'});
  run(`state.clusters = ${JSON.stringify([ordinary])}`);
  await run('refreshGPU()'); await run('refreshHostGPU()');
  assert.equal(requests.length, 0, 'ordinary clusters must not probe GPU APIs');
  run('inspectHostGPU()');
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(requests, ['/api/gpu/host'], 'explicit inspection must work without GPU clusters');
  run('state.hostRequested = false; state.hostGPU = null');
  requests.length = 0;
  run(`state.clusters = ${JSON.stringify([ordinary, gpu])}`);
  await run('refreshGPU()');
  assert.deepEqual(requests, ['/api/clusters/gpu/gpu']);
  assert.match(run('gpuResources(state.clusters[1])'), /Collecting GPU health/);
  handler = () => snapshot();
  await run('refreshGPU()');
  let body = run('gpuResources(state.clusters[1])');
  assert.match(body, /1\/1<\/b> GPU workers schedulable/);
  assert.match(body, /1 pending workload/);
  assert.match(body, /&lt;script&gt;bad&lt;\/script&gt;/);
  assert.doesNotMatch(body, /<script>bad/);
  assert.match(body, /not measured VRAM usage/);
  assert.match(body, /queue submission and transfer readback/);
  assert.match(body, /do not measure GPU utilization or shader correctness/);
  assert.match(body, /1Gi/);
  run("state.gpuExpanded.gpu = true");
  assert.match(run('gpuResources(state.clusters[1])'), /class="gpu-details" open/);
  run('render()');
  const summary = document.getElementById('gpu-details-gpu');
  summary.focus();
  run('render()');
  assert.notEqual(document.activeElement, summary, 'GPU summary must be a replacement DOM node');
  assert.equal(document.activeElement, document.getElementById('gpu-details-gpu'), 'GPU summary focus must survive polling redraw');
  element('memory').focus();
  run('render()');
  assert.equal(document.activeElement, element('memory'), 'redraw must not steal focus from the form');
  document.getElementById('gpu-details-gpu').focus();
  run(`state.gpu.gpu.capturedAt = '${new Date(Date.now() - 31000).toISOString()}'`);
  body = run('gpuResources(state.clusters[1])');
  assert.match(body, /Snapshot expired/); assert.doesNotMatch(body, /healthy|1Gi/);
  run('expireGPUSnapshots()'); assert.equal(run('state.gpu.gpu.stale'), true);
  assert.equal(run('state.gpuExpanded.gpu'), true);
  assert.equal(document.getElementById('gpu-details-gpu'), null, 'expired GPU details must be removed');
  assert.equal(document.activeElement, null, 'a removed summary must not restore focus elsewhere');
  run(`state.gpu.gpu = ${JSON.stringify(snapshot())}; state.gpu.gpu.state = 'stopped'`);
  assert.match(run('gpuResources(state.clusters[1])'), /GPU cluster is stopped/);
  assert.doesNotMatch(run('gpuResources(state.clusters[1])'), /healthy|1Gi/);
  run(`state.gpu.gpu = ${JSON.stringify(snapshot())}; state.gpu.gpu.capturedAt = 'not a date'`);
  assert.equal(run('snapshotExpired(state.gpu.gpu, GPU_MAX_AGE_MS)'), true);
  run(`state.gpu.gpu.capturedAt = '${new Date(Date.now() + 5000).toISOString()}'`);
  assert.equal(run('snapshotExpired(state.gpu.gpu, GPU_MAX_AGE_MS)'), true);
  handler = () => ({state: 'ready', capturedAt: now(), devices: [{name: '<Apple M1 Max>', metalSupport: 'Supported'}]});
  await run('refreshHostGPU()');
  assert.match(element('host-gpu').innerHTML, /&lt;Apple M1 Max&gt;/);
  assert.match(element('host-gpu').innerHTML, /not a native Metal execution service/);
  const hostCount = requests.filter(x => x === '/api/gpu/host').length;
  await run('refreshHostGPU()');
  assert.equal(requests.filter(x => x === '/api/gpu/host').length, hostCount, 'fresh hardware snapshots must avoid repeated probes');
  run(`state.hostGPU.capturedAt = '${new Date(Date.now() - 301000).toISOString()}'`);
  run('renderHostGPU()');
  assert.match(element('host-gpu').innerHTML, /Hardware snapshot expired/);
  assert.doesNotMatch(element('host-gpu').innerHTML, /&lt;Apple M1 Max&gt;/);
  assert.equal(element('memory').value, '10G');
  element('gpu').checked = true; element('gpu').listeners.change();
  assert.equal(element('memory').value, '10G', 'enabling GPU must preserve custom memory');
  assert.equal(element('cpus').value, '6', 'enabling GPU must preserve custom CPU count');
  element('gpu-inference-preset').listeners.click();
  assert.equal(element('memory').value, '4G', 'explicit preset click must apply');
  assert.equal(element('cpus').value, '6', 'preset must preserve a larger CPU count');
  element('cpus').value = '1'; element('cpus').listeners.input();
  assert.match(element('gpu-preset-status').textContent, /Custom VM sizing/);
  element('gpu-inference-preset').listeners.click();
  assert.equal(element('cpus').value, '4', 'preset must leave CPU headroom for the inference server and GPU agents');
  element('memory').value = '12G'; element('memory').listeners.input();
  element('gpu').checked = false; element('gpu').listeners.change();
  element('gpu').checked = true; element('gpu').listeners.change();
  assert.equal(element('memory').value, '12G', 'custom memory must survive repeated toggles');
  let resolveResponse;
  handler = () => new Promise(resolve => { resolveResponse = resolve; });
  run('delete state.gpu.gpu');
  const request = run('refreshGPU()');
  await new Promise(resolve => setImmediate(resolve));
  run("state.clusters[1].nodes[0].status = 'stopped'");
  resolveResponse(snapshot()); await request;
  assert.equal(run('state.gpu.gpu'), undefined, 'late responses must not reinstate stopped worker health');
  console.log('GPU dashboard behavior checks passed: opt-in inventory, GPU-only polling, accounting, escaping, staleness, expansion, keyboard focus, explicit sizing preset, late-response invalidation.');
})().catch(err => { console.error(err); process.exitCode = 1; });
