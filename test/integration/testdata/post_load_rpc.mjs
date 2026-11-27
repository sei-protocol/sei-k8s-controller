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
  let txCount = 0;
  let logCount = 0;
  for (let h = last; h >= Math.max(first, last - 499); h--) {
    const block = await read('eth_getBlockByNumber', [quantity(h), false]);
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
    scenarios.push({ name, method, params, expectedHash, metadata, verify(value) {
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
  return { scenarios, filter, corpus: { first: range.fromBlock, last: end, blocks: blocks.length, transactions: txCount, logs: logCount } };
}

async function checkedTransaction(read, tx, header) {
  const result = await read('eth_getTransactionByHash', [tx.hash]);
  assert.equal(result?.hash, tx.hash);
  assert.equal(result.blockHash, header.hash);
  return result;
}

function report(scenario, method, samples, fields) {
  const times = samples.map(sample => sample.ms).sort((a, b) => a - b);
  const percentile = q => times[Math.ceil(times.length * q) - 1];
  console.log(JSON.stringify({ scenario, method, ...fields, requests: times.length,
    responseBytes: samples.reduce((total, sample) => total + sample.bytes, 0),
    p50Ms: percentile(.5), p95Ms: times.length >= 20 ? percentile(.95) : null,
    p99Ms: times.length >= 100 ? percentile(.99) : null }));
}

async function measureFilter(request, filter, budgetMs) {
  const populated = [], drained = [];
  async function cycle(measure) {
    const { value: id } = await request('eth_newFilter', [filter.criteria]);
    assert.match(id, /^0x[0-9a-f]+$/i);
    try {
      const first = await request('eth_getFilterChanges', [id]);
      assert.deepEqual(first.value.map(logIdentity), filter.expected, 'first poll differs from receipts');
      const next = await request('eth_getFilterChanges', [id]);
      assert.deepEqual(next.value, [], 'drained filter replayed logs');
      if (measure) {
        populated.push({ ms: first.ms, bytes: first.bytes });
        drained.push({ ms: next.ms, bytes: next.bytes });
      }
    } finally {
      assert.equal((await request('eth_uninstallFilter', [id])).value, true);
    }
  }
  await cycle(false);
  const start = performance.now();
  do { await cycle(true); } while (performance.now() - start < budgetMs);
  for (const [scenario, samples, logs] of [['populated', populated, filter.expected.length], ['drained', drained, 0]]) {
    report('filter-' + scenario, 'eth_getFilterChanges', samples, { ...filter.criteria, logs });
  }
}

export async function runPostLoad(endpoint, first, last, budgetMs = 600_000) {
  let id = 0;
  async function request(method, params) {
    const body = JSON.stringify({ jsonrpc: '2.0', id: ++id, method, params });
    const start = performance.now();
    const response = await fetch(endpoint, { method: 'POST', headers: { 'content-type': 'application/json' },
      body, signal: AbortSignal.timeout(35_000) });
    assert.ok(response.ok, method + ': HTTP ' + response.status);
    const bytes = await response.arrayBuffer();
    const ms = performance.now() - start;
    const envelope = JSON.parse(new TextDecoder().decode(bytes));
    assert.equal(envelope.id, id, 'mismatched JSON-RPC response');
    assert.ok(!envelope.error, method + ': ' + JSON.stringify(envelope.error));
    assert.ok(Object.hasOwn(envelope, 'result'), method + ': missing result');
    return { value: envelope.result, bytes: bytes.byteLength, ms };
  }
  // Tendermint can report the end height before the EVM receipt watermark
  // catches up. Poll for this exact height before sealing the corpus.
  const readyBy = performance.now() + 120_000;
  while (BigInt((await request('eth_blockNumber', [])).value) < BigInt(last)) {
    assert.ok(performance.now() < readyBy, 'receipt follower did not reach load height ' + last);
    await sleep(1000);
  }
  const { scenarios, filter, corpus } = await prepareScenarios(async (method, params) => (await request(method, params)).value, first, last);
  console.log(JSON.stringify({ phase: 'post-load-rpc', endpoint, corpus, fingerprint: digest(scenarios.map(s => [s.name, s.params, s.expectedHash])), scenarios: scenarios.length + 2 }));
  const timePerScenario = budgetMs / (scenarios.length + 2);
  await measureFilter(request, filter, 2 * timePerScenario);
  for (const scenario of scenarios) {
    async function sample() {
      const { value, ms, bytes } = await request(scenario.method, scenario.params);
      scenario.verify(value);
      return { ms, bytes };
    }
    const first = await sample();
    const samples = [];
    const start = performance.now();
    do { samples.push(await sample()); } while (performance.now() - start < timePerScenario);
    const elapsed = performance.now() - start;
    const trace = scenario.method.startsWith('debug_trace');
    report(scenario.name, scenario.method, samples, { ...scenario.metadata,
      ...(trace ? { tracer: callTracer.tracer, firstObservedMs: first.ms } : {}),
      rps: samples.length * 1000 / elapsed });
  }
}
