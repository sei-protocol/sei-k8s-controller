// Port of arctic-tests/tests/rpc_benchmark/loadScenarios.ts for the Harbor
// integration harness. All reads use the same receipt follower and sealed heights.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { performance } from 'node:perf_hooks';
import { setTimeout as sleep } from 'node:timers/promises';

const quantity = height => '0x' + height.toString(16);
const logFields = ['address', 'topics', 'data', 'blockHash', 'blockNumber', 'transactionHash', 'transactionIndex', 'logIndex', 'removed'];

function canonical(value) {
  if (typeof value === 'string' && value.startsWith('0x')) return value.toLowerCase();
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.keys(value).sort().map(key => [key, canonical(value[key])]));
  }
  return value;
}

function digest(value) {
  return createHash('sha256').update(JSON.stringify(canonical(value))).digest('hex');
}

function logIdentity(log) {
  for (const key of logFields) assert.ok(Object.hasOwn(log, key), 'missing log field ' + key);
  assert.ok(Array.isArray(log.topics), 'missing log topics');
  assert.equal(log.removed, false, 'removed log in sealed block');
  return logFields.map(key => log[key]);
}

// Pinned requests are checked on every repeat. Keep one observed summary
// per scenario so logging does not grow with either RPS or response size.
function logSummary(criteria, expectedCount, logs) {
  return { ...criteria, blockSpan: Number(BigInt(criteria.toBlock) - BigInt(criteria.fromBlock) + 1n),
    expectedLogsPerRequest: expectedCount, returnedLogsPerRequest: logs.length,
    blocksWithMatches: new Set(logs.map(log => log.blockNumber)).size,
    exampleLog: logs[0] ?? null };
}

const transferTopic = '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';
const callTracer = { tracer: 'callTracer', timeout: '25s' };

function traceScenario(name, method, params, metadata, verify) {
  return { name, method, params: [...params, callTracer], metadata, verify };
}

function verifyFrame(frame, tx) {
  assert.equal(frame?.type, 'CALL');
  assert.ok(!frame.error, 'load transaction trace failed');
  assert.equal(frame.from.toLowerCase(), tx.from.toLowerCase());
  assert.equal(frame.to.toLowerCase(), tx.to.toLowerCase());
  assert.equal(frame.input.toLowerCase(), tx.input.toLowerCase());
  assert.ok(BigInt(frame.gasUsed) > 0n);
}

export async function prepareScenarios(read, first, last) {
  assert.ok(Number.isSafeInteger(first) && first > 0 && Number.isSafeInteger(last) && last >= first, 'invalid load window');
  const blocks = [];
  const maxTransactions = 100_000;
  // The receipt follower caps eth_getLogs results at 10,000 even for a bounded range.
  const maxLogs = 10_000;
  let headersRead = 0;
  let txCount = 0;
  let logCount = 0;
  for (let h = last; h >= Math.max(first, last - 499); h--) {
    const block = await read('eth_getBlockByNumber', [quantity(h), false]);
    headersRead++;
    assert.equal(block?.number, quantity(h), 'missing load block ' + h);
    assert.ok(Array.isArray(block.transactions));
    const count = block.transactions.length;
    if (!blocks.length && !count) continue; // sender has stopped, chain still advances
    assert.ok(count <= maxTransactions, 'block exceeds transaction budget');
    if (txCount + count > maxTransactions) break;
    const receipts = await read('eth_getBlockReceipts', [block.number]);
    assert.ok(Array.isArray(receipts));
    assert.equal(receipts.length, count, 'receipt count at ' + block.number);
    const logs = [];
    for (const [i, receipt] of receipts.entries()) {
      assert.equal(receipt.transactionHash.toLowerCase(), block.transactions[i].toLowerCase());
      assert.equal(receipt.blockHash.toLowerCase(), block.hash.toLowerCase());
      assert.ok(Array.isArray(receipt.logs));
      for (const log of receipt.logs) logs.push(log);
    }
    assert.ok(logs.length <= maxLogs, 'block exceeds log budget');
    if (logCount + logs.length > maxLogs) break;
    if (blocks.length) assert.equal(blocks.at(-1).header.parentHash, block.hash, 'non-contiguous load blocks');
    blocks.push({ header: block, sampleReceipt: receipts[Math.floor(count / 2)], logs });
    logCount += logs.length;
    txCount += count;
  }
  assert.ok(txCount, 'load window contains no transactions');
  blocks.reverse();
  const scenarios = [];
  const seal = (name, method, params, expected, metadata) => {
    assert.notEqual(expected, null, 'missing reference for ' + name);
    const fingerprint = value => {
      if (method !== 'eth_getLogs') return digest(value);
      assert.ok(Array.isArray(value), 'expected a log array');
      return digest(value.map(logIdentity)); // preserve order and duplicates
    };
    const expectedHash = fingerprint(expected);
    const expectedLogs = method === 'eth_getLogs' ? expected.length : undefined;
    scenarios.push({ name, method, params, expectedHash, expectedLogs, metadata, verify(value) {
      if (method === 'eth_getLogs') {
        assert.ok(Array.isArray(value));
        assert.equal(value.length, expectedLogs, name + ' returned an unexpected log count');
      }
      assert.equal(fingerprint(value), expectedHash, name + ' differs from its sealed reference');
    } });
  };
  const end = blocks.at(-1).header.number;
  const logs = blocks.flatMap(block => block.logs);
  const range = { fromBlock: blocks[0].header.number, toBlock: end };
  for (const width of new Set([1, 10, 100, blocks.length].filter(n => n <= blocks.length))) {
    const suffix = blocks.slice(-width);
    seal('logs-' + width + '-blocks', 'eth_getLogs', [{ fromBlock: suffix[0].header.number, toBlock: end }], suffix.flatMap(block => block.logs));
  }
  const sampleLog = logs.find(log => log.topics.length);
  if (sampleLog) {
    const address = sampleLog.address.toLowerCase();
    const topic = sampleLog.topics[0].toLowerCase();
    const hasTopic = (log, value) => log.topics[0]?.toLowerCase() === value;
    seal('logs-address', 'eth_getLogs', [{ ...range, address }], logs.filter(log => log.address.toLowerCase() === address));
    seal('logs-topic', 'eth_getLogs', [{ ...range, topics: [topic] }], logs.filter(log => hasTopic(log, topic)));
    let absent = '0x' + 'ff'.repeat(32);
    while (logs.some(log => hasTopic(log, absent))) absent = '0x' + digest(absent);
    seal('logs-no-match', 'eth_getLogs', [{ ...range, topics: [absent] }], []);
  }

  const populated = blocks.filter(b => b.header.transactions.length);
  const addresses = new Set();
  for (const { header, sampleReceipt } of new Set([populated[0], populated[Math.floor((populated.length - 1) / 2)], populated.at(-1)])) {
    const full = await read('eth_getBlockByNumber', [header.number, true]);
    assert.equal(full?.hash, header.hash);
    assert.deepEqual(full.transactions.map(tx => tx.hash), header.transactions);
    seal('full-block-' + header.number, 'eth_getBlockByNumber', [header.number, true], full);
    seal('block-hashes-' + header.number, 'eth_getBlockByHash', [header.hash, false], header);
    const idx = Math.floor(header.transactions.length / 2);
    const tx = full.transactions[idx];
    // The full block and its receipts supply independent lookup references.
    seal('transaction-' + header.number, 'eth_getTransactionByHash', [tx.hash], await checkedTransaction(read, tx, header));
    seal('receipt-' + header.number, 'eth_getTransactionReceipt', [tx.hash], sampleReceipt);
    for (const address of [tx.from, tx.to]) {
      if (/^0x[\da-f]{40}$/i.test(address)) addresses.add(address.toLowerCase());
    }
  }
  for (const address of [...addresses].slice(0, 4)) {
    for (const method of ['eth_getBalance', 'eth_getCode']) {
      const params = [address, end];
      const value = await read(method, params);
      assert.match(value, /^0x[\da-f]*$/i);
      seal(method + '-' + address, method, params, value);
    }
  }
  // Use a token and holder that actually participated in this workload.
  const transfer = logs.find(l => l.topics.length === 3 && l.topics[0].toLowerCase() === transferTopic);
  assert.ok(transfer, 'steady-state corpus has no ERC-20 Transfer');
  const call = { to: transfer.address, data: '0x70a08231' + transfer.topics[2].slice(2) };
  for (const height of new Set([range.fromBlock, end])) {
    const value = await read('eth_call', [call, height]);
    assert.match(value, /^0x[0-9a-f]{64}$/i, 'balanceOf must return one ABI word');
    const position = height === end ? 'last' : 'first';
    seal('call-balanceOf-' + position, 'eth_call', [call, height], value, { height, contract: call.to });
    if (height === end) {
      scenarios.push(traceScenario('trace-call-balanceOf', 'debug_traceCall', [call, height], { height }, frame => {
        assert.equal(frame?.type, 'CALL');
        assert.ok(!frame.error, 'balanceOf trace failed');
        assert.equal(frame.to.toLowerCase(), call.to.toLowerCase());
        assert.equal(frame.output.toLowerCase(), value.toLowerCase());
      }));
    }
  }

  const tx = await read('eth_getTransactionByHash', [transfer.transactionHash]);
  assert.equal(tx?.hash, transfer.transactionHash);
  assert.equal(tx.blockHash, transfer.blockHash);
  scenarios.push(traceScenario('trace-token-transaction', 'debug_traceTransaction', [tx.hash],
    { height: transfer.blockNumber, txHash: tx.hash }, frame => verifyFrame(frame, tx)));

  const ranked = [...populated].sort((a, b) => Number(BigInt(a.header.gasUsed) - BigInt(b.header.gasUsed)));
  const seen = new Set();
  for (const percentile of [50, 90]) {
    const header = ranked[Math.ceil(percentile / 100 * ranked.length) - 1].header;
    if (seen.has(header.hash)) continue;
    seen.add(header.hash);
    const metadata = { height: header.number, blockHash: header.hash, gasUsed: header.gasUsed, transactions: header.transactions.length };
    let reference;
    function verifyBlock(entries) {
      assert.ok(Array.isArray(entries));
      assert.deepEqual(entries.map(entry => entry.txHash.toLowerCase()), header.transactions.map(hash => hash.toLowerCase()));
      for (const entry of entries) {
        assert.ok(!entry.error && entry.result?.type, 'missing block trace frame');
      }
      // Both lookup methods must agree on every frame, including internal calls.
      const actual = digest(entries);
      reference ??= actual;
      assert.equal(actual, reference, 'block traces differ');
    }
    for (const [method, key] of [['debug_traceBlockByNumber', header.number], ['debug_traceBlockByHash', header.hash]]) {
      scenarios.push(traceScenario(method + '-gas-p' + percentile, method, [key], metadata, verifyBlock));
    }
  }

  const filterBlock = blocks.find(block => block.logs.length);
  const filter = { criteria: { fromBlock: filterBlock.header.number, toBlock: filterBlock.header.number },
    expected: filterBlock.logs.map(logIdentity) };
  return { scenarios, filter, corpus: { first: range.fromBlock, last: end, headersRead, blocks: blocks.length, transactions: txCount, logs: logCount } };
}

async function checkedTransaction(read, tx, header) {
  const result = await read('eth_getTransactionByHash', [tx.hash]);
  assert.equal(result?.hash, tx.hash);
  assert.equal(result.blockHash, header.hash);
  return result;
}

function suiteGroup(method) {
  if (method === 'eth_getFilterChanges') return 'filters';
  if (method === 'eth_getLogs') return 'logs';
  if (method.startsWith('debug_')) return 'traces';
  if (['eth_call', 'eth_getBalance', 'eth_getCode'].includes(method)) return 'state';
  return 'blocks-transactions-receipts';
}

function report(scenario, method, samples, fields) {
  const times = samples.map(sample => sample.ms).sort((a, b) => a - b);
  const percentile = q => times[Math.ceil(times.length * q) - 1];
  console.log(JSON.stringify({ suite: 'post-load-rpc', group: suiteGroup(method), scenario, method, ...fields, requests: times.length,
    responseBytes: samples.reduce((total, sample) => total + sample.bytes, 0),
    p50Ms: percentile(.5), p95Ms: times.length >= 20 ? percentile(.95) : null,
    p99Ms: times.length >= 100 ? percentile(.99) : null }));
}

async function measureFilter(request, filter, budgetMs) {
  const populated = [], drained = [];
  let firstSummary, drainedSummary;
  async function cycle(measure) {
    const { value: id } = await request('eth_newFilter', [filter.criteria]);
    assert.match(id, /^0x[0-9a-f]+$/i);
    try {
      const first = await request('eth_getFilterChanges', [id]);
      assert.deepEqual(first.value.map(logIdentity), filter.expected, 'first poll differs from receipts');
      const next = await request('eth_getFilterChanges', [id]);
      assert.deepEqual(next.value, [], 'drained filter replayed logs');
      if (measure) {
        firstSummary ??= logSummary(filter.criteria, filter.expected.length, first.value);
        drainedSummary ??= logSummary(filter.criteria, 0, next.value);
        populated.push({ ms: first.ms, bytes: first.bytes });
        drained.push({ ms: next.ms, bytes: next.bytes });
      }
    } finally {
      assert.equal((await request('eth_uninstallFilter', [id])).value, true);
    }
  }
  await cycle(false);
  const startedAt = new Date().toISOString();
  const start = performance.now();
  do { await cycle(true); } while (performance.now() - start < budgetMs);
  const endedAt = new Date().toISOString();
  report('filter-populated', 'eth_getFilterChanges', populated, { startedAt, endedAt, ...firstSummary, logs: firstSummary.returnedLogsPerRequest });
  report('filter-drained', 'eth_getFilterChanges', drained, { startedAt, endedAt, ...drainedSummary, logs: drainedSummary.returnedLogsPerRequest });
}

function rpcClient(endpoint) {
  let id = 0;
  return async function request(method, params, expectedError) {
    const body = JSON.stringify({ jsonrpc: '2.0', id: ++id, method, params });
    const start = performance.now();
    const response = await fetch(endpoint, { method: 'POST', headers: { 'content-type': 'application/json' },
      body, signal: AbortSignal.timeout(35_000) });
    assert.ok(response.ok, method + ': HTTP ' + response.status);
    const bytes = await response.arrayBuffer();
    const ms = performance.now() - start;
    const envelope = JSON.parse(new TextDecoder().decode(bytes));
    assert.equal(envelope.id, id, 'mismatched JSON-RPC response');
    if (expectedError) {
      assert.equal(envelope.error?.code, -32000, method + ': expected limit error, got ' + JSON.stringify(envelope.error));
      assert.match(envelope.error.message, expectedError);
      assert.ok(!Object.hasOwn(envelope, 'result'));
      return { error: envelope.error, ms, bytes: bytes.byteLength };
    }
    assert.ok(!envelope.error, method + ': ' + JSON.stringify(envelope.error));
    assert.ok(Object.hasOwn(envelope, 'result'), method + ': missing result');
    return { value: envelope.result, bytes: bytes.byteLength, ms };
  };
}

function followerMetricsURL(endpoint) {
  const url = new URL('/metrics', endpoint);
  url.port = '26660';
  return url;
}

async function processSnapshot(url) {
  const response = await fetch(url, { signal: AbortSignal.timeout(5000) });
  assert.ok(response.ok, 'metrics endpoint HTTP ' + response.status);
  const body = await response.text();
  const gauges = {};
  const names = new Set(['process_cpu_seconds_total', 'process_resident_memory_bytes', 'process_start_time_seconds']);
  for (const line of body.split('\n')) {
    if (!line.startsWith('process_')) continue;
    const sample = /^(\w+)(?:\{.*\})?\s+(\S+)/.exec(line);
    if (sample && names.has(sample[1])) {
      assert.ok(!Object.hasOwn(gauges, sample[1]), 'ambiguous follower metric ' + sample[1]);
      gauges[sample[1]] = Number(sample[2]);
    }
  }
  for (const name of names) assert.ok(Number.isFinite(gauges[name]), 'missing follower metric ' + name);
  return { timestamp: new Date().toISOString(), ...gauges };
}

// All heights exist. A rare topic keeps the at-limit positive control small.
export async function runLimitChecks(endpoint, end, maxBlocks, control, durationMs, metricsURL = followerMetricsURL(endpoint)) {
  assert.ok(Number.isSafeInteger(maxBlocks) && maxBlocks > 0 && end > maxBlocks, 'insufficient sealed history for range boundary');
  const request = rpcClient(endpoint);
  const toBlock = quantity(end);
  const range = width => [{ fromBlock: quantity(end - width + 1), toBlock, topics: ['0x' + 'ff'.repeat(32)] }];
  const percentages = Array.from({ length: 101 }, (_, i) => i);
  const fee = percentiles => ['0x1', toBlock, percentiles];
  const allowedLogs = await request('eth_getLogs', range(maxBlocks));
  assert.ok(Array.isArray(allowedLogs.value));
  const allowedFees = await request('eth_feeHistory', fee(percentages.slice(1)));
  assert.equal(allowedFees.value.reward?.[0]?.length, 100, 'at-limit reward count');
  console.log(JSON.stringify({ suite: 'post-load-rpc', group: 'limits', event: 'boundary-controls',
    timestamp: new Date().toISOString(), endpoint, toBlock, allowedBlockSpan: maxBlocks,
    logsReturned: allowedLogs.value.length, returnedRewardPercentiles: allowedFees.value.reward[0].length }));
  const cases = [
    { name: 'logs-range-plus-one', method: 'eth_getLogs', params: range(maxBlocks + 1),
      error: new RegExp('block range too large.*maximum allowed is ' + maxBlocks + ' blocks'), limit: maxBlocks, requested: maxBlocks + 1 },
    { name: 'fee-percentile-plus-one', method: 'eth_feeHistory', params: fee([101]),
      error: /invalid reward percentiles.*between 0 and 100/i, limit: 100, requested: 101 },
    { name: 'fee-percentile-count-plus-one', method: 'eth_feeHistory', params: fee(percentages),
      error: /rewardPercentiles length must be less than or equal to 100/i, limit: 100, requested: 101 },
  ];
  const valid = { name: 'valid-one-block', method: 'eth_getLogs', params: control.params };
  for (const [stage, share] of [['baseline', 1/6], ['exercise', 2/3], ['recovery', 1/6]]) {
    const active = stage === 'exercise' ? [...cases, valid] : [valid];
    const samples = new Map(active.map(c => [c.name, []]));
    const startedAt = new Date().toISOString();
    const metrics = [];
    async function snapshot() {
      const reading = await processSnapshot(metricsURL);
      metrics.push(reading);
      console.log(JSON.stringify({ suite: 'post-load-rpc', group: 'limits', stage, event: 'follower-metrics', ...reading }));
    }
    await snapshot();
    let lastScrape = performance.now();
    const deadline = lastScrape + durationMs * share;
    do {
      const tick = performance.now();
      for (const c of active) {
        try {
          const r = await request(c.method, c.params, c.error);
          if (!c.error) control.verify(r.value);
          samples.get(c.name).push({ ms: r.ms, bytes: r.bytes, error: r.error });
        } catch (err) {
          console.log(JSON.stringify({ suite: 'post-load-rpc', group: 'limits', stage, scenario: c.name,
            event: 'unexpected-response', timestamp: new Date().toISOString(), verifiedBeforeFailure: samples.get(c.name).length, error: err.message }));
          throw err;
        }
      }
      if (performance.now() - lastScrape >= 5000) {
        await snapshot();
        lastScrape = performance.now();
      }
      // One round per second at most and one in-flight request. Never catch up missed rounds.
      const wait = Math.min(deadline, tick + 1000) - performance.now();
      if (wait > 0) await sleep(wait);
    } while (performance.now() < deadline);
    await snapshot();
    const first = metrics[0], last = metrics.at(-1);
    assert.equal(last.process_start_time_seconds, first.process_start_time_seconds, 'receipt follower restarted');
    const seconds = (Date.parse(last.timestamp) - Date.parse(first.timestamp)) / 1000;
    const cpu = last.process_cpu_seconds_total - first.process_cpu_seconds_total;
    for (const c of active) {
      const rows = samples.get(c.name);
      report(c.name, c.method, rows, { group: 'limits', stage, startedAt, endedAt: last.timestamp,
        limit: c.limit, requested: c.requested, params: c.params, expectedRejections: c.error ? rows.length : 0,
        successfulReads: c.error ? 0 : rows.length, rps: seconds > 0 ? rows.length / seconds : null, observedError: rows[0].error,
        followerMeanCpuCores: seconds > 0 ? cpu / seconds : null,
        followerPeakRssBytes: Math.max(...metrics.map(m => m.process_resident_memory_bytes)), followerSamples: metrics.length });
    }
  }
}

// Shuffle relative heights reproducibly so neither the target nor reference is
// traversed sequentially. Reserve the pinned suite's 500-height setup window.
export async function measureDistinctBlocks(request, reference, first, last, budgetMs) {
  const end = last - first >= 500 ? last - 500 : last;
  const cases = [
    { name: 'logs-1-block', method: 'eth_getLogs', width: 1 },
    { name: 'full-block', method: 'eth_getBlockByNumber', width: 1 },
    { name: 'block-trace', method: 'debug_traceBlockByNumber', width: 1 },
    { name: 'logs-10-blocks', method: 'eth_getLogs', width: 10 },
  ];
  // Assign disjoint spans first, then shuffle each case independently so a
  // ten-block log lookup cannot prime a later block or trace lookup.
  for (const c of cases) {
    c.targets = [];
    c.samples = { firstRead: [], immediateRepeat: [] };
  }
  let cursor = first;
  while (cursor <= end) {
    for (const c of cases) {
      if (cursor + c.width - 1 > end) continue;
      c.targets.push({ height: cursor, order: digest(['rpc-height-order-v1', c.name, cursor - first]) });
      cursor += c.width;
    }
  }
  for (const c of cases) c.targets.sort((a, b) => a.order.localeCompare(b.order));
  const targets = [];
  for (let i = 0; i < Math.max(...cases.map(c => c.targets.length)); i++) {
    for (const c of cases) if (c.targets[i]) targets.push({ c, height: c.targets[i].height });
  }
  const startedAt = new Date().toISOString();
  const start = performance.now();
  let count = 0;
  for (const { c, height } of targets) {
    const { method } = c;
    const h = quantity(height);
    const range = { fromBlock: h, toBlock: quantity(height + c.width - 1) };
    let params = [range];
    if (method === 'eth_getBlockByNumber') params = [h, true];
    if (method === 'debug_traceBlockByNumber') params = [h, callTracer];

    const initial = await request(method, params);
    const repeat = await request(method, params);
    assert.deepEqual(repeat.value, initial.value, method + ' repeat differs at ' + height);
    let details;
    if (method === 'eth_getLogs') {
      let referenceLogs;
      if (c.width === 1) {
        const { value: receipts } = await reference('eth_getBlockReceipts', [h]);
        referenceLogs = receipts.flatMap(receipt => receipt.logs);
      } else {
        referenceLogs = (await reference(method, params)).value;
      }
      const expected = referenceLogs.map(logIdentity);
      assert.deepEqual(initial.value.map(logIdentity), expected, 'rotated logs differ from receipts at ' + height);
      details = logSummary(range, expected.length, initial.value);
    } else if (method === 'eth_getBlockByNumber') {
      const { value: block } = await reference(method, params);
      assert.equal(block?.number, h);
      assert.deepEqual(initial.value, block, 'rotated block differs across followers at ' + height);
      details = { transactions: block.transactions.length, gasUsed: block.gasUsed };
    } else {
      const { value: block } = await reference('eth_getBlockByNumber', [h, false]);
      assert.equal(block?.number, h);
      assert.deepEqual(initial.value.map(entry => entry.txHash), block.transactions);
      for (const entry of initial.value) assert.ok(!entry.error && entry.result?.type, 'missing rotated block trace frame');
      details = { transactions: block.transactions.length, gasUsed: block.gasUsed };
    }
    const pair = c.samples;
    pair.firstRead.push({ ms: initial.ms, bytes: initial.bytes });
    pair.immediateRepeat.push({ ms: repeat.ms, bytes: repeat.bytes });
    console.log(JSON.stringify({ suite: 'post-load-rpc', event: 'height-pair', method, workload: c.name, height, depthFromLoadEnd: last - height,
      firstReadMs: initial.ms, immediateRepeatMs: repeat.ms, firstResponseBytes: initial.bytes, repeatResponseBytes: repeat.bytes, ...details }));
    count++;
    if (count >= cases.length && performance.now() - start >= budgetMs) break;
  }
  const endedAt = new Date().toISOString();
  for (const c of cases) {
    const pair = c.samples;
    if (!pair.firstRead.length) continue;
    for (const [access, rows] of Object.entries(pair)) {
      report('historical-' + c.name + '-' + access, c.method, rows, { startedAt, endedAt, access,
        blockSpan: c.width, distinctTargets: rows.length, distinctHeights: rows.length * c.width, eligibleTargets: c.targets.length, poolExhausted: rows.length === c.targets.length });
    }
  }
}

export async function runPostLoad(endpoint, first, last, budgetMs = 600_000, maxBlocks = 2000, metricsURL = followerMetricsURL(endpoint), referenceEndpoint = endpoint) {
  const request = rpcClient(endpoint);
  // Tendermint can report the end height before the EVM receipt watermark
  // catches up. Poll for this exact height before sealing the corpus.
  const readyBy = performance.now() + 120_000;
  while (BigInt((await request('eth_blockNumber', [])).value) < BigInt(last)) {
    assert.ok(performance.now() < readyBy, 'receipt follower did not reach load height ' + last);
    await sleep(1000);
  }
  const pairStart = performance.now();
  await measureDistinctBlocks(request, rpcClient(referenceEndpoint), first, last, budgetMs / 5);
  const pairElapsed = performance.now() - pairStart;
  const { scenarios, filter, corpus } = await prepareScenarios(async (method, params) => (await request(method, params)).value, first, last);
  console.log(JSON.stringify({ phase: 'post-load-rpc', endpoint, corpus, fingerprint: digest(scenarios.map(s => [s.name, s.params, s.expectedHash])), readScenarios: scenarios.length + 2, limitStages: ['baseline', 'exercise', 'recovery'] }));
  const control = scenarios.find(s => s.name === 'logs-1-blocks');
  await runLimitChecks(endpoint, Number(corpus.last), maxBlocks, control, budgetMs, metricsURL);
  const timePerScenario = Math.max(0, budgetMs - pairElapsed) / (scenarios.length + 2);
  await measureFilter(request, filter, 2 * timePerScenario);
  const groups = ['logs', 'blocks-transactions-receipts', 'state', 'traces'];
  const ordered = groups.flatMap(group => scenarios.filter(s => suiteGroup(s.method) === group));
  assert.equal(ordered.length, scenarios.length);
  for (const scenario of ordered) {
    let observedLogs;
    async function sample(measured) {
      const { value, ms, bytes } = await request(scenario.method, scenario.params);
      scenario.verify(value);
      if (measured && scenario.method === 'eth_getLogs') {
        observedLogs ??= logSummary(scenario.params[0], scenario.expectedLogs, value);
      }
      return { ms, bytes };
    }
    const first = await sample(false);
    const startedAt = new Date().toISOString();
    const samples = [];
    const start = performance.now();
    do { samples.push(await sample(true)); } while (performance.now() - start < timePerScenario);
    const elapsed = performance.now() - start;
    const trace = scenario.method.startsWith('debug_trace');
    report(scenario.name, scenario.method, samples, { startedAt, endedAt: new Date().toISOString(), ...scenario.metadata, ...observedLogs,
      ...(trace ? { tracer: callTracer.tracer, firstObservedMs: first.ms } : {}),
      rps: samples.length * 1000 / elapsed });
  }
}
