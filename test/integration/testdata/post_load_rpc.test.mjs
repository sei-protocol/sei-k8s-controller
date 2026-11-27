import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { measureDistinctBlocks, prepareScenarios, runPostLoad } from './post_load_rpc.mjs';

const address = '0x' + '11'.repeat(20);
const topic = '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';
const quantity = n => '0x' + n.toString(16);
const hash = n => '0x' + n.toString(16).padStart(64, '0');
const heights = [10, 11, 12];
const transaction = n => ({ hash: hash(n + 100), blockHash: hash(n), from: address, to: address, input: '0x1234' });
const log = n => ({ address, topics: [topic, hash(1), hash(2)], data: '0x', blockHash: hash(n), blockNumber: quantity(n),
  transactionHash: transaction(n).hash, transactionIndex: '0x0', logIndex: '0x0', removed: false });
const receipt = n => ({ blockHash: hash(n), transactionHash: transaction(n).hash, logs: [log(n)] });

const frame = { type: 'CALL', from: address, to: address, input: '0x1234', gasUsed: '0x100', output: hash(42) };
const filters = new Map();
let nextFilter = 0;
function rpc(method, [arg, full]) {
  const n = typeof arg === 'string' ? Number(BigInt(arg)) : 0;
  switch (method) {
    case 'eth_blockNumber': return quantity(15);
    case 'eth_getBlockByNumber':
    case 'eth_getBlockByHash':
      return { number: quantity(n), hash: hash(n), gasUsed: quantity(n * 1000), parentHash: hash(n - 1),
        transactions: heights.includes(n) ? [full ? transaction(n) : transaction(n).hash] : [] };
    case 'eth_getBlockReceipts': return heights.includes(n) ? [receipt(n)] : [];
    case 'eth_getTransactionByHash': return transaction(n - 100);
    case 'eth_getTransactionReceipt': return receipt(n - 100);
    case 'eth_newFilter': {
      const id = quantity(++nextFilter);
      filters.set(id, rpc('eth_getLogs', [arg]));
      return id;
    }
    case 'eth_getFilterChanges': {
      assert.ok(filters.has(arg));
      const response = filters.get(arg);
      filters.set(arg, []);
      return response;
    }
    case 'eth_uninstallFilter': return filters.delete(arg);
    case 'eth_call':
      assert.equal(arg.data, '0x70a08231' + hash(2).slice(2));
      assert.ok(heights.includes(Number(full)));
      return hash(42);
    case 'debug_traceCall':
    case 'debug_traceTransaction': return frame;
    case 'debug_traceBlockByNumber':
    case 'debug_traceBlockByHash': return heights.includes(n) ? [{ txHash: transaction(n).hash, result: frame }] : [];
    case 'eth_getBalance': return '0x1';
    case 'eth_getCode': return '0x';
    case 'eth_getLogs': return heights.filter(h => h >= Number(arg.fromBlock) && h <= Number(arg.toBlock)
      && (!arg.topics || arg.topics[0] === topic)).map(log);
    default: throw Error('Unexpected method ' + method);
  }
}

test('seals the actual load suffix and checks log order, multiplicity and receipt identity', async () => {
  const { scenarios, corpus } = await prepareScenarios(rpc, 10, 15);
  assert.deepEqual(corpus, { first: '0xa', last: '0xc', headersRead: 6, blocks: 3, transactions: 3, logs: 3 });
  for (const s of scenarios) s.verify(rpc(s.method, s.params));
  const byHash = scenarios.find(s => s.method === 'debug_traceBlockByHash');
  const [entry] = rpc(byHash.method, byHash.params);
  assert.throws(() => byHash.verify([{ ...entry, txHash: hash(0) }]));
  assert.throws(() => byHash.verify([{ ...entry, result: { ...frame, output: '0x' } }]), /traces differ/);
  const traceCall = scenarios.find(s => s.method === 'debug_traceCall');
  assert.throws(() => traceCall.verify({ ...frame, output: '0x' }));
  const wide = scenarios.find(s => s.name === 'logs-3-blocks');
  assert.throws(() => wide.verify([log(12), log(11), log(10)]));
  assert.throws(() => wide.verify([log(10), log(10), log(12)]));
  await assert.rejects(prepareScenarios(rpc, 14, 15), /no transactions/);
  await assert.rejects(prepareScenarios((m, p) => m === 'eth_getBlockReceipts' ? [] : rpc(m, p), 10, 12), /receipt count/);
});

test('runs every scenario over HTTP and fails on a successful RPC with incorrect data', async t => {
  const reports = new Map();
  t.mock.method(console, 'log', line => {
    const row = JSON.parse(line);
    reports.set(row.scenario, row);
  });
  let corrupt;
  const measured = [];
  const server = createServer(async (req, resp) => {
    if (req.url === '/metrics') {
      resp.end('process_cpu_seconds_total{chain_id="fixture"} 2\nprocess_resident_memory_bytes 1024\nprocess_start_time_seconds 10\n');
      return;
    }
    let body = '';
    for await (const chunk of req) body += chunk;
    const { id, method, params } = JSON.parse(body);
    measured.push(method);
    let result, error;
    const [arg, , rewards] = params;
    if (method === 'eth_feeHistory') {
      if (rewards.length > 100) error = 'rewardPercentiles length must be less than or equal to 100';
      else if (rewards.at(-1) > 100) error = 'invalid reward percentiles: must be ascending and between 0 and 100';
      else result = { reward: [rewards.map(() => '0x0')] };
    } else if (method === 'eth_getLogs' && Number(arg.toBlock) - Number(arg.fromBlock) >= 4) {
      if (corrupt === 'accepted') result = [];
      else error = corrupt === 'wrong-error' ? 'server overloaded' : 'block range too large (5), maximum allowed is 4 blocks';
    } else {
      result = corrupt === method ? [] : rpc(method, params);
    }
    resp.end(JSON.stringify({ jsonrpc: '2.0', id, ...(error ? { error: { code: -32000, message: error } } : {result}) }));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const url = 'http://127.0.0.1:' + server.address().port;
  const run = () => runPostLoad(url, 10, 15, 0, 4, url + '/metrics');
  await run();
  const rejected = reports.get('logs-range-plus-one');
  assert.equal(rejected.expectedRejections, 1);
  assert.equal(rejected.requested, 5);
  assert.equal(rejected.stage, 'exercise');
  assert.equal(rejected.followerPeakRssBytes, 1024);
  assert.equal(reports.get('fee-percentile-count-plus-one').observedError.code, -32000);
  assert.equal(reports.get('valid-one-block').stage, 'recovery');
  for (const [name, span, count] of [['logs-3-blocks', 3, 3], ['logs-address', 3, 3], ['logs-topic', 3, 3],
    ['logs-no-match', 3, 0], ['filter-populated', 1, 1], ['filter-drained', 1, 0]]) {
    const row = reports.get(name);
    assert.equal(row.blockSpan, span);
    assert.equal(row.expectedLogsPerRequest, count);
    assert.equal(row.returnedLogsPerRequest, count);
    assert.equal(row.blocksWithMatches, count);
    assert.deepEqual(row.exampleLog, count ? log(10) : null);
  }
  assert.equal(reports.get('logs-address').address, address);
  assert.deepEqual(reports.get('logs-topic').topics, [topic]);
  assert.ok(measured.includes('eth_getTransactionReceipt'));
  assert.ok(measured.includes('debug_traceBlockByHash'));
  assert.equal(measured.filter(m => m === 'eth_getFilterChanges').length, 4);
  assert.equal(filters.size, 0);
  corrupt = 'eth_getFilterChanges';
  await assert.rejects(run(), /first poll differs/);
  assert.equal(filters.size, 0);
  for (corrupt of ['accepted', 'wrong-error']) await assert.rejects(run(), /expected limit error|block range too large/);
  corrupt = 'eth_getLogs';
  await assert.rejects(run(), /rotated logs differ|unexpected log count/);
});

test('narrows the contiguous suffix before a wide query would exceed the follower log limit', async () => {
  const read = (method, params) => {
    if (method !== 'eth_getBlockReceipts') return rpc(method, params);
    const n = Number(params[0]);
    const sample = receipt(n);
    sample.logs = Array.from({ length: 5001 }, (_, i) => ({ ...log(n), logIndex: quantity(i) }));
    return [sample];
  };
  const { corpus } = await prepareScenarios(read, 10, 12);
  assert.deepEqual(corpus, { first: '0xc', last: '0xc', headersRead: 2, blocks: 1, transactions: 1, logs: 5001 });
});

test('pairs first and repeated reads before reference queries and exhausts distinct heights without recycling', async t => {
  const events = [], reports = [];
  t.mock.method(console, 'log', line => reports.push(JSON.parse(line)));
  const client = source => async (method, params) => {
    const height = Number(params[0]?.fromBlock ?? params[0]);
    events.push([source, method, height]);
    return { value: rpc(method, params), ms: 2, bytes: 100 };
  };
  await measureDistinctBlocks(client('target'), client('reference'), 10, 515, 30_000);
  const pairs = reports.filter(r => r.event === 'height-pair');
  assert.equal(pairs.length, 6);
  assert.deepEqual(pairs.map(p => p.height).sort((a,b) => a-b), [10,11,12,13,14,15]);
  for (const [index, pair] of pairs.entries()) {
    const [first, repeat, ref] = events.slice(index * 3, index * 3 + 3);
    assert.deepEqual(first, ['target', pair.method, pair.height]);
    assert.deepEqual(first, repeat);
    assert.equal(ref[0], 'reference');
    assert.equal(ref[2], pair.height);
  }
  const rows = reports.filter(r => r.scenario);
  assert.equal(rows.length, 6);
  for (const row of rows) {
    assert.equal(row.requests, 2);
    assert.equal(row.distinctHeights, 2);
    assert.equal(row.poolExhausted, true);
  }
  reports.length = 0;
  await measureDistinctBlocks(client('target'), client('reference'), 10, 535, 30_000);
  const covered = reports.filter(r => r.event === 'height-pair').flatMap(row =>
    Array.from({length: row.blockSpan ?? 1}, (_, offset) => row.height + offset));
  assert.equal(covered.length, 26);
  assert.equal(new Set(covered).size, 26);
  assert.equal(reports.find(r => r.scenario === 'historical-logs-10-blocks-firstRead').distinctHeights, 20);
  await assert.rejects(measureDistinctBlocks(client('target'), async () => ({value: []}), 10, 10, 0), /rotated logs differ/);
});
