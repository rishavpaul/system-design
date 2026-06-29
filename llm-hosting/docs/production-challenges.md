# Production Challenges & Problem Statements

> The PLAN gets a model serving and benchmarked. This doc is about everything that
> breaks, surprises you, or costs money **once real traffic hits**. Each section is a
> challenge (why it's hard) followed by **problem statements** — framed as solvable,
> measurable exercises with acceptance criteria. Treat them as the project's backlog.
>
> Difficulty: 🟢 foundational · 🟡 intermediate · 🔴 advanced

---

## 1. The GPU is a fixed, expensive, lumpy resource

**Why it's hard.** Unlike stateless web servers, a GPU serves a bounded number of
concurrent sequences gated by **VRAM (the KV cache)**, not CPU. You can't "just add a
replica" cheaply — the smallest unit of scale is a whole GPU costing $0.30–$3+/hr. Idle
GPUs burn money; saturated GPUs drop latency off a cliff. The economics are lumpy and
unforgiving.

**Problem statements**
- 🟢 **P1.1 — Saturation curve.** Find the exact concurrency at which P95 TTFT crosses
  your SLO for Qwen3-8B on one 4090. Deliver a latency-vs-throughput curve and identify
  the "knee." *Accept:* a chart + the single concurrency number you'd set as the per-GPU cap.
- 🟡 **P1.2 — KV-cache budgeting.** Derive (then empirically confirm) how many concurrent
  4k-context sequences fit before OOM, as a function of `--gpu-memory-utilization` and
  `--max-model-len`. *Accept:* a formula that predicts measured capacity within ±15%.
- 🔴 **P1.3 — Right-sizing.** For a fixed workload + SLO, prove which GPU gives the lowest
  $/Mtok (a cheaper GPU at high utilization often beats a pricier one half-idle).
  *Accept:* a ranked table across ≥3 GPUs with the winner justified.

---

## 2. Cold starts vs. idle cost (the scale-to-zero dilemma)

**Why it's hard.** Keeping a GPU warm 24/7 is the #1 way to waste money (~$245/mo for a
4090). But scaling to zero means **cold starts**: pulling a multi-GB image, downloading
model weights, and loading them into VRAM can take **30s–several minutes**. You're forced
to trade idle cost against tail latency, and bursty traffic makes both worse.

**Problem statements**
- 🟡 **P2.1 — Cold-start budget.** Measure end-to-end cold start on RunPod Serverless and
  cut it by ≥50% (e.g. baked weights into the image, model caching, smaller image,
  flashboot). *Accept:* before/after cold-start P50/P95.
- 🟡 **P2.2 — Warm-pool policy.** Design a keep-warm policy (min workers, idle timeout)
  for a given traffic pattern that meets a TTFT SLO while minimizing monthly cost.
  *Accept:* a cost-vs-latency table for ≥3 policies + your recommendation.
- 🔴 **P2.3 — Bursty traffic.** Simulate a spike (1→50 concurrent in 10s). Quantify how
  many requests miss SLO during scale-up and propose a mitigation (pre-warm on signal,
  request queue with admission control). *Accept:* SLO-miss % before/after.

---

## 3. Latency is multi-dimensional (and the averages lie)

**Why it's hard.** "Latency" splits into **TTFT** (prefill + queue) and **TPOT/ITL**
(decode speed), and they trade off against throughput and against each other. A long
prompt from one user can stall short requests behind it (head-of-line blocking).
Streaming makes P50 look fine while P99 users have a terrible time. Averages hide all of it.

**Problem statements**
- 🟢 **P3.1 — Percentile discipline.** Reproduce a workload where mean TTFT looks healthy
  but P99 violates SLO; explain the cause. *Accept:* the data + root-cause write-up.
- 🟡 **P3.2 — Mixed workload fairness.** With a mix of long (8k) and short (256) prompts,
  show head-of-line blocking, then fix it with **chunked prefill** and measure the short-
  request P95 improvement. *Accept:* before/after percentiles.
- 🔴 **P3.3 — Prefix caching payoff.** For an agent/RAG workload with a shared 2k system
  prompt, quantify TTFT and throughput gains from prefix caching. *Accept:* % improvement
  + the break-even prompt-sharing ratio where it stops helping.

---

## 4. Capacity planning, autoscaling & queueing

**Why it's hard.** Traffic is non-uniform (daily peaks, spikes). You must decide how many
GPUs, when to add/remove them, and **what to do with requests that arrive while saturated**
— queue them (latency), shed them (errors), or autoscale (cold-start lag). GPU autoscaling
is slow and coarse compared to CPU autoscaling.

**Problem statements**
- 🟡 **P4.1 — Admission control.** Implement a request queue with a max depth + timeout in
  front of vLLM. Decide the policy: reject-fast vs. queue-and-wait. *Accept:* behavior
  under overload documented (latency, error rate, no OOM crashes).
- 🟡 **P4.2 — Autoscaling signal.** Choose the right scaling metric (queue depth? GPU KV
  util? pending tokens?) and show it triggers scale-up *before* SLO breaks, not after.
  *Accept:* a trace showing the metric leading the SLO breach.
- 🔴 **P4.3 — Capacity model.** Build a back-of-envelope model that, given req/s + token
  distributions + SLO, outputs required GPU count, and validate it against a load test
  within ±20%. *Accept:* the model + validation run.

---

## 5. Reliability, failure modes & graceful degradation

**Why it's hard.** GPUs fail in ugly ways: **CUDA OOM** kills the server mid-batch, a
single bad request can crash the process taking down *all* in-flight requests, drivers
hang, spot instances get reclaimed with little warning. A request can run for minutes, so
retries and timeouts are subtle. Statelessness assumptions from web services don't hold.

**Problem statements**
- 🟢 **P5.1 — OOM repro & guardrail.** Trigger a CUDA OOM with an oversized request, then
  prevent it via input validation + max-token caps so one request can't crash the server.
  *Accept:* the attack request now returns a clean 4xx, server stays up.
- 🟡 **P5.2 — Health checks that mean something.** Design liveness/readiness probes that
  detect a *hung* GPU (not just a live process) and auto-restart. *Accept:* inject a hang,
  show automatic recovery + the MTTR.
- 🟡 **P5.3 — Timeout & retry policy.** Define client/server timeouts and a retry policy
  that doesn't amplify load or double-bill long generations. *Accept:* documented policy +
  a test showing no retry storm under failure.
- 🔴 **P5.4 — Spot reclamation.** On a spot/interruptible GPU, drain in-flight requests on
  the termination signal and fail over. *Accept:* simulated reclaim with zero dropped
  requests (or a measured, bounded number).

---

## 6. Observability for a token factory

**Why it's hard.** Standard APM doesn't know about tokens, KV cache, or batch occupancy.
You need LLM-native signals — tokens/s, queue depth, KV-cache utilization, batch size,
TTFT/TPOT histograms — and you need **per-tenant cost attribution**, which no infra metric
gives you for free. Without it you're optimizing blind.

**Problem statements**
- 🟢 **P6.1 — The dashboard.** Stand up Prometheus + Grafana on vLLM's `/metrics` showing
  TTFT/TPOT percentiles, throughput, GPU + KV-cache util, and running $/Mtok. *Accept:* a
  dashboard-as-code you'd trust on call.
- 🟡 **P6.2 — Cost attribution.** Attribute GPU cost to individual API keys/tenants from
  token usage. *Accept:* a per-tenant $/period report reconciled against the GPU bill (±10%).
- 🟡 **P6.3 — SLO alerting.** Define burn-rate alerts on TTFT/TPOT SLOs that page before
  users complain, without flapping. *Accept:* alert fires on injected regression, silent
  otherwise.

---

## 7. Multi-tenancy, fairness & rate limiting

**Why it's hard.** On shared infra, one heavy user can starve everyone (noisy neighbor).
Rate limiting by request count is meaningless when one request can be 200k tokens —
you must limit by **tokens** and **concurrency**. Fairness across tenants on a single GPU
is genuinely hard.

**Problem statements**
- 🟢 **P7.1 — Token-aware rate limiting.** Implement per-key limits on tokens/min + max
  concurrency at the edge (not request count). *Accept:* a heavy key gets throttled while
  a light key is unaffected.
- 🔴 **P7.2 — Fairness under contention.** With two tenants hammering one GPU, enforce a
  fairness policy (e.g. weighted) so neither is fully starved. *Accept:* measured share of
  throughput matches the target weights.
- 🟡 **P7.3 — Quota & overage.** Add per-tenant quotas with a defined overage behavior
  (block / degrade / queue). *Accept:* documented behavior + test at the quota boundary.

---

## 8. Security & abuse resistance

**Why it's hard.** A public GPU endpoint is a juicy, expensive target. Beyond normal API
security you face **resource-exhaustion attacks** (context bombs, token floods that run up
your bill), prompt injection through your MCP tools, and the risk of leaking weights or
keys. The blast radius is financial, not just availability.

**Problem statements**
- 🟢 **P8.1 — Never-expose invariant.** Prove the raw vLLM port is unreachable from the
  internet; all traffic flows through the authenticated TLS edge. *Accept:* a port scan +
  an unauthenticated request both fail.
- 🟡 **P8.2 — Cost-DoS defense.** Defend against a token-flood / max-context attack so an
  attacker can't run up the GPU bill. *Accept:* attack capped; measured worst-case spend
  per malicious key is bounded.
- 🟡 **P8.3 — MCP tool hardening.** Ensure your MCP tools validate/escape inputs and can't
  be coerced (via injected prompts) into unbounded generation or unintended actions.
  *Accept:* a red-team prompt set that fails to break out.
- 🟢 **P8.4 — Secrets hygiene.** No keys/HF tokens in images, git, or logs. *Accept:* a
  scan of image + repo + logs comes back clean.

---

## 9. Model & config lifecycle (deploys without downtime)

**Why it's hard.** Swapping a model or vLLM version means re-downloading weights and a
slow reload — you can't hot-reload in place. Quantization or config changes can subtly
**change output quality**, so you need eval gates, not just "it boots." Rollbacks must be
fast when a new model regresses.

**Problem statements**
- 🟡 **P9.1 — Zero-downtime model swap.** Roll from model/config A to B with no failed
  requests (blue-green or rolling). *Accept:* a swap under live load with 0 errors.
- 🟡 **P9.2 — Quality gate.** Build an eval set so a quantization/version change is blocked
  if quality drops beyond a threshold. *Accept:* a regression is caught automatically and
  the deploy is halted.
- 🟢 **P9.3 — Fast rollback.** Demonstrate rollback to the previous good config in under a
  defined budget (e.g. < 2 min). *Accept:* timed rollback drill.

---

## 10. Reproducibility & environment drift

**Why it's hard.** The GPU stack is a tower of version-coupled pieces — **CUDA, drivers,
PyTorch, vLLM, the model revision** — and a mismatch silently changes performance or
breaks startup. "Works on my pod" is rampant. Benchmarks are worthless if the environment
isn't pinned.

**Problem statements**
- 🟢 **P10.1 — Pin everything.** Produce a fully pinned, reproducible image (CUDA/PyTorch/
  vLLM/model SHA) such that a teammate gets identical benchmark numbers cold. *Accept:* an
  independent rerun reproduces results within noise.
- 🟡 **P10.2 — IaC from zero.** Recreate the entire stack from code (SkyPilot/Terraform)
  with one command, no manual steps. *Accept:* a clean-account/clean-pod bring-up succeeds
  unattended.

---

## 11. Quality, correctness & evaluation

**Why it's hard.** Performance work (quantization, speculative decoding, sampling params)
can quietly **degrade output quality**, and quality is hard to measure objectively.
Without an eval harness you optimize cost while silently shipping a worse model.

**Problem statements**
- 🟡 **P11.1 — Quant quality cost.** Quantify the quality delta (on a task set) between
  FP16 and 4-bit AWQ, alongside the $/Mtok savings. *Accept:* a quality-vs-cost trade
  table that supports a recommendation.
- 🔴 **P11.2 — Determinism & drift.** Investigate output variability across batch sizes /
  GPU types for the same input+seed and document what is and isn't reproducible. *Accept:*
  a written finding on sources of non-determinism.

---

## 12. Cost engineering as a first-class discipline

**Why it's hard.** The bill is the product of a dozen coupled choices (model, quant, GPU,
batch, utilization, warm-pool, spot vs on-demand). Optimizing one in isolation can make
another worse. The only honest metric is **$/Mtok at a stated SLO**, and getting there
requires disciplined, controlled experiments.

**Problem statements**
- 🟡 **P12.1 — The headline number.** For one realistic workload + SLO, produce the
  cheapest config as a defensible $/Mtok, with the reasoning. *Accept:* one sentence backed
  by reproducible data — the project's flagship artifact.
- 🔴 **P12.2 — Spot economics.** Quantify the savings *and* the reliability cost of running
  on spot/interruptible GPUs vs on-demand for a given availability target. *Accept:* a
  $/availability trade-off with a recommendation.
- 🔴 **P12.3 — GPU-native vs hyperscaler TCO.** Compare true total cost (incl. egress,
  ops overhead, quotas) of RunPod vs AWS for the same SLO. *Accept:* a TCO decision matrix
  with the "when each wins" conclusion.

---

## How to use this backlog

- Each phase in [PLAN.md](../PLAN.md) naturally surfaces a subset of these. Pull the
  matching problem statements in as you go rather than all at once.
- Start with 🟢 items in §1, §5, §6, §8 — they prevent the most painful (and expensive)
  early mistakes.
- Every problem statement has an **acceptance criterion** on purpose: in this domain,
  "it works" is meaningless without a number attached.
```
