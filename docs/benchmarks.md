# Benchmarks

Nobody in this space publishes allocation figures, so here are skyl's.

**Read the allocation columns, not the timings.** `B/op` and `allocs/op` are
deterministic — identical across runs on any machine. `ns/op` varies with load,
CPU and thermal state; the numbers below came from a busy laptop and are a
**ceiling on what you should expect**, not a target.

Reproduce with:

```bash
go test -run='^$' -bench=. -benchmem ./internal/...
```

Measured on `Intel(R) Core(TM) i7-4790 @ 3.60GHz`, linux/amd64, `GOMAXPROCS=8`.
That is a Haswell part from 2014 — a current server core will be materially
faster.

---

## Per token, per stream

These run once for every token of every concurrent stream. They are the floor
under everything else.

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `ReaderNext` (256 SSE frames) | 76,199 | 94,240 | 258 |
| `ReaderMultilineData` (128 multi-line frames) | 270,458 | 81,952 | 642 |
| `StreamChunkDecode` (one chunk) | 5,786 | 664 | 14 |

**`ReaderNext`** parses 256 realistic OpenAI delta frames, so the per-frame cost
is about **368 bytes and one allocation**, at 376 MB/s. That is the SSE reader
alone.

**`StreamChunkDecode`** is the JSON decode of a single streaming chunk: **664
bytes, 14 allocations**. The whole response struct is allocated even for a
one-token frame, which is where most of that goes.

Together, budget roughly **1 KB and ~15 allocations per token, per stream**. At
1,000 concurrent streams producing 50 tokens a second, that is around 50 MB/s of
garbage — comfortably within reach of Go's collector, but worth knowing before
you size a fleet.

**`ReaderMultilineData`** is the same reader against frames carrying eight
continuation `data:` lines each. At 54 MB/s it is **five times slower per byte**
than the single-line case, because the spec requires joining them. Providers
that use multi-line frames cost more; do not assume the 376 MB/s figure applies
universally.

## Per request

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `BuildPayloadAndMarshal` — 2 turns | 20,517 | 4,579 | 62 |
| — 20 turns | 68,885 | 22,662 | 188 |
| — 100 turns | 280,528 | 107,689 | 748 |
| `CompleteDecode` (~1 KB body) | 22,976 | 1,696 | 16 |

**Payload construction is per *attempt*, not per request.** A retry rebuilds it
from scratch. With the default 3 retries, a 100-turn conversation that fails
twice and succeeds on the third attempt has allocated **three times 107 KB**
before a single byte of the successful response arrives. If you run long
conversations against a flaky provider, that is where the garbage comes from.

Cost is essentially linear in conversation length, which is the expected shape:
roughly 1 KB and 7 allocations per turn.

**`CompleteDecode`** is negligible next to the multi-second upstream call it
follows. It is here so nobody optimises it.

## Tool-argument accumulation

Providers stream tool-call arguments as many small fragments, so the adapter
reassembles them.

| Fragments | ns/op | B/op | allocs/op |
|---|---|---|---|
| 8 | 2,002 | 512 | 9 |
| 64 | 5,641 | 3,584 | 13 |
| 512 | 22,058 | 34,560 | 19 |

**This is linear, and it used not to be.** The original implementation joined
fragments with `+=`, which reallocates and copies the whole accumulated string
every time. At 512 fragments that allocated **2.2 MB to assemble 34 KB** — 65×
more memory and 516 allocations instead of 19.

The benchmark found it, a `strings.Builder` fixed it, and the benchmark stays as
the guard. It is the clearest argument in this repository for measuring a hot
path rather than reasoning about it: nothing in the code looked wrong.

---

## What is not measured

- **The gateway.** Its cost is dominated by the upstream call, and a benchmark
  of the routing layer would measure noise.
- **`provider/anthropic`.** It delegates decoding to the vendor SDK.
- **End-to-end latency.** That is the provider's, not skyl's. skyl's overhead is
  the microseconds above against a call that takes seconds.

## Interpreting these against your own numbers

If skyl appears in a profile at more than a percent or two of a request's cost,
something unusual is happening and it is worth an issue. The library sits between
your code and a network call that takes a million times longer; it should be
invisible.

The exception is the per-token path under very high stream concurrency, which is
the one place these numbers can add up — and the reason they are measured at all.
