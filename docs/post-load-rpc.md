# Post-load RPC benchmark

`TestNightlyValidatorHistory` provisions a fresh chain, drives its 30-minute transaction workload, then benchmarks the receipt follower over EVM HTTP. The benchmark checks responses against sealed heights. It emits JSON lines in the RPC Job log. Each of four steps has five minutes of measurement time. They cover paired historical reads, boundary checks, the weighted read mix, and the isolated read scenarios. The individual methods share the isolated step. The RPC target is 20 minutes, with readiness, fixture preparation and the last in-flight operations adding wall time. Each step emits its own start and end times and actual duration.

| Group | Workload | What a passing result checks |
|---|---|---|
| Paired historical reads | First read and immediate repeat on each selected block or range | Every pair agrees and is checked against the other follower. |
| Limits | Five minutes across baseline, exercise and recovery | At-limit requests succeed. Each over-limit request returns the expected JSON-RPC code and reason. The valid read continues to return the receipt-derived logs. |
| Weighted read mix | Five minutes at 10 offered requests/s across contract, log and block reads | Each response matches its pinned fixture. Per-method rows record scheduled, verified and missed requests. |
| Filters | Create, poll twice, uninstall | The first poll matches receipts and the drained poll returns an empty array. Each poll has its own latency distribution. |
| Logs | Unfiltered block ranges, address, topic and no-match filters | Every response matches receipt logs including ordering and duplicates. Each row records the requested span, blocks with matches, observed count and one sample event. |
| Blocks, transactions and receipts | Block-by-number, block-by-hash, transaction and receipt lookups | Results match the pinned block and transaction identities. |
| State | Balance, bytecode and ERC-20 balanceOf reads | Repeated reads return the sealed value at each sampled height. |
| Traces | Transaction, call and block traces by number and hash | Frames match the selected subject. Both block lookups agree on ordered traces for median- and high-gas blocks. First-observed latency is recorded separately. |

## Workload definition and reporting

The [versioned profile](../test/integration/testdata/post_load_rpc_profile.json) sets the common step duration, read-mix rate, method weights and fixture selectors. The first report event records the profile and both follower endpoints. The runner resolves the selectors against the load-generated corpus and records the method parameters, reference digests and expected log counts. Each chain supplies its own heights, addresses and transaction hashes.

The read mix follows Nethermind's read-heavy proportions. For the default 3,000 scheduled calls, 1,800 read a token balance, 900 query filtered logs and 300 fetch full blocks. Each method rotates through its sealed fixtures. One client issues requests on a fixed schedule. If a request takes more than a slot, the runner skips expired slots and records the missed arrivals. Each result has a target and achieved rate, completed count, response bytes and latency percentiles. An incorrect response fails the run with its fixture and slot. The 10 requests/s setting is a Sei regression workload, with throughput capacity measured separately if required. [Nethermind read-heavy workload](https://github.com/NethermindEth/json-bench/blob/main/config/benchmark/read-heavy.yaml)

The isolated block workloads ask `eth_getBlockByNumber` for hashes and full transactions at identical heights, recording transaction count, response bytes and timing for each projection. The isolated log cases report range width and actual matches for selective, all-match and empty queries. These controls make response size and filtering cost visible alongside timing. [Nethermind block workloads](https://github.com/NethermindEth/json-bench/blob/main/config/benchmark/block-workloads.md) · [Nethermind log workloads](https://github.com/NethermindEth/json-bench/blob/main/config/benchmark/log-workloads.md)

The scheduled `TestNightlyBenchmark/steady-state` uses RPC for transaction submission, receipt tracking and chain checks. The dedicated post-load workloads run under `TestNightlyValidatorHistory`. The broader nightly schedule also includes functional release tests on separate chains.

## Measuring cache effects

Before corpus preparation, the runner samples disjoint sealed heights from the load window in a reproducibly shuffled order. On a normal load run it reserves the newest 500 heights for the pinned scenarios. It times an initial request on the receipt follower and immediately repeats the identical request there. Then it uses the send follower as the reference. It rotates through one-block and ten-block log queries, full blocks and block traces with `callTracer`. No height or range is reused across pairs. The runner spaces out pairs to cover the sampling window when the finite pool is small.

The `height-pair` records contain the height, depth from the end of load, both latencies and both response sizes. Log rows include the requested range and counts. Full block and trace rows include transaction count and gas used. The summary gives separate first-read and immediate-repeat latency distributions for each workload. First-read means the benchmark has yet to request that target. Block execution, receipt tracking, trace precomputation and the OS page cache can all have touched it. A storage-level cache miss requires node-side evidence.

The Solana Foundation uses distinct historical slots and compares returned block hashes across providers. Nethermind's Ethereum suite uses parameter variations, cross-client checks and a separate time-to-first-correct-response benchmark for newly arriving blocks. [Solana Foundation methodology](https://github.com/solana-foundation/rpc-latency-monitor/blob/main/rpc-monitor/README.md) · [Nethermind JSON-Bench](https://github.com/NethermindEth/json-bench)

## Continuous responsiveness and rate settings

Once provisioning completes, the harness independently calls `eth_blockNumber` on every validator and each RPC follower every five seconds, with a two-second timeout. The harness log records one `rpc-responsiveness` event per observation, including the node, phase, UTC start time, latency, outcome, HTTP status when available, and height on success. These samples cover transaction load, validator checks and the entire post-load RPC job. Join timestamps to RPC step windows to compare success rates, successful-read latency and head progress. Timeouts and errors remain visible even while the benchmark waits on another node.

The ephemeral validator-history network and its two standalone followers set `evm.rate_limiting_enabled = false` and `evm.large_log_query_rps = 0` in app.toml. This disables the per-IP admission limiter and the separate large-log-query rate limiter. Block-span and result-size bounds still define the rejection workloads. The weighted mix's offered rate controls the client schedule.

## Additional Nethermind controls

The runner measures all logs over 8 and 16 blocks, and a selective query for one workload token, event signature and indexed sender. Block-receipt reads are timed alongside individual receipts from the same selected blocks. The full ordered block receipt response must match the reference follower. Transaction traces sample first, middle and last indexes in the most populated of the three full-block fixtures. Each eligible call transaction reports its index and block size; coincident indexes run once. These cases use Sei-generated blocks and transaction hashes. [Upstream log workloads](https://github.com/NethermindEth/json-bench/blob/21d6ff1712f59e4fd3304a98e5f95c16312fa824/config/benchmark/log-workloads.md) · [Upstream trace workloads](https://github.com/NethermindEth/json-bench/blob/21d6ff1712f59e4fd3304a98e5f95c16312fa824/config/benchmark/trace-workloads.md)

## Reading the limits phase

The runner first checks an accepted 2,000-block log query with a selective topic and a one-block fee-history query with 100 reward percentiles including percentile 100. The follower configuration and runner share the block limit. All range endpoints are existing heights.

| Stage | Budget | Requests per round |
|---|---:|---|
| Baseline | 50 s | One valid, pinned one-block log read |
| Exercise | 200 s | A 2,001-block log query, a fee-history percentile of 101, a fee-history list of 101 valid percentiles, and the same valid log read |
| Recovery | 50 s | The same valid log read |

The client starts at most one round per second, with one request in flight. Delayed rounds reduce achieved throughput. Each expected rejection must arrive in a valid HTTP response with JSON-RPC code `-32000` and its matching reason. A timeout, a transport error, an unrelated error or an accepted over-limit query fails the run. The runner logs the failing case and how many earlier responses it verified.

During each stage the client samples the follower's process CPU counter, resident memory and process start time at the stage boundaries and approximately every five seconds. Stage summaries report the CPU delta divided by elapsed time in cores, peak sampled RSS, sample count and achieved requests per second. The one-block control has its own latency distribution in every stage. The CPU and RSS figures include the follower's ordinary chain work. Match UTC stage times with the Harbor pod metrics for the same chain when investigating a regression.

Use the group, scenario, stage where present, method, sample count and measured workload shape when comparing runs. Percentiles use complete HTTP response times. P95 requires at least 20 samples and p99 requires 100. Setup and response verification fall outside the individual request latency. This suite measures serial reads and paced rejection handling. Capacity at higher concurrency requires a separate experiment.
