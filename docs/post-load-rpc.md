# Post-load RPC benchmark

`TestNightlyValidatorHistory` provisions a fresh chain, drives its 30-minute transaction workload, then benchmarks the receipt follower over EVM HTTP. The benchmark checks responses against sealed heights. It emits JSON lines in the RPC Job log. The ten-minute measurement budget includes one minute for limits and nine minutes for the other groups. Corpus preparation, warmups and setup add wall time.

| Group | Workload | What a passing result checks |
|---|---|---|
| Limits | One minute across baseline, exercise and recovery | At-limit requests succeed. Each over-limit request returns the expected JSON-RPC code and reason. The valid read continues to return the receipt-derived logs. |
| Filters | Create, poll twice, uninstall | The first poll matches receipts and the drained poll returns an empty array. Each poll has its own latency distribution. |
| Logs | Unfiltered block ranges, address, topic and no-match filters | Every response matches receipt logs including ordering and duplicates. Each row records the requested span, blocks with matches, observed count and one sample event. |
| Blocks, transactions and receipts | Block-by-number, block-by-hash, transaction and receipt lookups | Results match the pinned block and transaction identities. |
| State | Balance, bytecode and ERC-20 balanceOf reads | Repeated reads return the sealed value at each sampled height. |
| Traces | Transaction, call and block traces by number and hash | Frames match the selected subject. Both block lookups agree on ordered traces for median- and high-gas blocks. First-observed latency is recorded separately. |

## Reading the limits phase

The runner first checks an accepted 2,000-block log query with a selective topic and a one-block fee-history query with 100 reward percentiles including percentile 100. The follower configuration and runner share the block limit. All range endpoints are existing heights.

| Stage | Budget | Requests per round |
|---|---:|---|
| Baseline | 10 s | One valid, pinned one-block log read |
| Exercise | 40 s | A 2,001-block log query, a fee-history percentile of 101, a fee-history list of 101 valid percentiles, and the same valid log read |
| Recovery | 10 s | The same valid log read |

The client starts at most one round per second, with one request in flight. Delayed rounds reduce achieved throughput. Each expected rejection must arrive in a valid HTTP response with JSON-RPC code `-32000` and its matching reason. A timeout, a transport error, an unrelated error or an accepted over-limit query fails the run. The runner logs the failing case and how many earlier responses it verified.

During each stage the client samples the follower's process CPU counter, resident memory and process start time at the stage boundaries and approximately every five seconds. Stage summaries report the CPU delta divided by elapsed time in cores, peak sampled RSS, sample count and achieved requests per second. The one-block control has its own latency distribution in every stage. The CPU and RSS figures include the follower's ordinary chain work. Match UTC stage times with the Harbor pod metrics for the same chain when investigating a regression.

Use the group, scenario, stage where present, method, sample count and measured workload shape when comparing runs. Percentiles use complete HTTP response times. P95 requires at least 20 samples and p99 requires 100. Setup and response verification fall outside the individual request latency. This suite measures serial reads and paced rejection handling. Capacity at higher concurrency requires a separate experiment.
