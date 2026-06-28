# Self-Hosting an Open-Source LLM: A Learn-by-Building Plan

> **Project goal:** Build an open-source reference project that teaches engineers how to
> deploy an open-weight LLM to cloud GPU infrastructure, expose it as a public API,
> connect it to Claude via the **Model Context Protocol (MCP)**, and rigorously
> **benchmark and cost-optimize** the inference stack.
>
> **Audience:** Engineers and teams who want to host models for their enterprise and need
> to understand the *real-life complexities* — not a toy demo.
>
> **Status:** Planning. This document is the master plan; each phase becomes its own
> directory + runnable code as the project is built out.

---

## 0. Decisions locked in (from project kickoff)

| Decision | Choice | Why |
|---|---|---|
| Cloud / GPU platform | **GPU-native cloud first (RunPod), AWS as the "enterprise" comparison** | Cheapest path to learning + per-second billing; AWS later to feel real org constraints (IAM, VPC, quotas) |
| Model size target | **7–8B class as the workhorse**, with a deliberate detour to a **32B quantized** model | 7–8B fits one 24GB GPU and teaches the *whole* stack cheaply; 32B forces you to learn quantization, KV-cache pressure, and multi-GPU/tensor-parallel realities |
| Serving stack | **vLLM** (primary) | Industry-standard, OpenAI-compatible, continuous batching + PagedAttention — the right tool for throughput and cost work |
| Claude integration | **Custom MCP server wrapping the model's OpenAI-compatible endpoint** | Claude speaks MCP, not raw OpenAI APIs; this is the bridge |

> These are starting points, not dogma. Part of the learning is re-running the
> benchmarks and revising them. Every choice below has a "why" and an "alternative."

---

## 1. Learning objectives (what "done" means)

By the end you should be able to explain, *from having done it*:

1. **Model economics** — how model size, quantization, context length, and batch size
   translate into VRAM, latency, throughput, and dollars.
2. **Serving internals** — what continuous batching, PagedAttention, prefix caching,
   and tensor parallelism actually do, and when each one moves the needle.
3. **Cloud GPU operations** — provisioning, cold starts, autoscaling-to-zero, spot vs
   on-demand, and the trade-offs between a GPU-native cloud and a hyperscaler.
4. **Public API design for inference** — auth, rate limiting, TLS, streaming, and the
   blast radius of exposing a GPU box to the internet.
5. **MCP** — how to build a server, what makes a good tool surface, and how Claude
   consumes it.
6. **Benchmarking discipline** — defining SLOs, measuring TTFT / TPOT / throughput,
   load-testing methodology, and turning measurements into a **cost-per-million-tokens**
   number you can defend.

---

## 2. Target architecture

```
                                        ┌────────────────────────────────────────────┐
                                        │              Cloud GPU host                  │
   ┌──────────┐    MCP (stdio /         │   ┌───────────────┐      ┌───────────────┐  │
   │  Claude   │◄──── streamable ──────►│   │  MCP server    │────► │  vLLM server   │  │
   │ (Desktop/  │      HTTP)            │   │ (FastMCP)      │ HTTP │  OpenAI /v1    │  │
   │  Code)     │                       │   │  tools:        │      │  Qwen3-8B      │  │
   └──────────┘                        │   │  ask_model,    │      │  + KV cache    │  │
        ▲                               │   │  summarize...  │      │  on GPU        │  │
        │ public HTTPS                  │   └───────────────┘      └───────────────┘  │
        │                               │           ▲                      ▲           │
   ┌──────────────┐                    │           │ API key / rate limit  │           │
   │ Edge / gateway│◄──── HTTPS ───────┼───────────┘                      │           │
   │ (Caddy/Nginx │   internet         │   ┌──────────────────────────────┘           │
   │  + auth +    │                    │   │  Prometheus  ─►  Grafana dashboards       │
   │  TLS)        │                    │   │  (tokens/s, TTFT, GPU util, $/Mtok)       │
   └──────────────┘                    └────────────────────────────────────────────┘
        ▲
        │
   ┌──────────────┐
   │ Load tester   │  (GuideLLM / vllm bench / k6) — runs the benchmarking suite
   └──────────────┘
```

**Data path:** Claude → MCP server → vLLM OpenAI endpoint → GPU. The public edge
(reverse proxy) terminates TLS and enforces auth/rate limits so the raw vLLM port is
never exposed.

---

## 3. Tooling & stack choices (with alternatives)

| Layer | Primary choice | Alternatives to benchmark against | Notes |
|---|---|---|---|
| Model | **Qwen3-8B** (Apache 2.0) | Llama-3.1-8B, Mistral-7B, DeepSeek-R1-Distill-8B, Qwen3-32B (AWQ/GPTQ) | Apache 2.0 = no license friction for an OSS teaching project. Strong, broadly-quantized, huge ecosystem. |
| Serving | **vLLM** | TGI, SGLang, Ollama (DX baseline), llama.cpp | Ollama makes a great "naive baseline" to show how much vLLM wins on throughput. |
| Quantization | **AWQ / GPTQ (4-bit)**, FP8 on Hopper | bitsandbytes, GGUF (llama.cpp) | Drives the 32B detour and most of the cost savings. |
| Edge/gateway | **Caddy** (auto-TLS) | Nginx, Traefik, cloud LB | Caddy gives free Let's Encrypt TLS with ~3 lines of config. |
| MCP server | **FastMCP (Python)** | TypeScript MCP SDK | Same language as the rest of the stack; supports stdio + streamable HTTP. |
| Benchmarking | **GuideLLM** + `vllm bench serve` | k6, Locust, llmperf | GuideLLM is purpose-built for LLM SLO/cost analysis. |
| Metrics | **Prometheus + Grafana** | cloud-native monitoring | vLLM exposes `/metrics` natively. |
| IaC | **SkyPilot** (cloud-agnostic) + **Terraform** (for AWS) | Pulumi, raw CLI | SkyPilot makes the "deploy to cheapest cloud" story real and reproducible. |

---

## 4. Cost landscape (June 2026 reference numbers)

These are the numbers that make the project concrete. **Re-verify before spending** —
GPU prices have fallen 60–75% in ~18 months and keep moving.

| GPU | VRAM | RunPod (approx) | Lambda (approx) | Good for |
|---|---|---|---|---|
| RTX 4090 | 24 GB | ~$0.34/hr | — | 7–8B full-precision, 32B 4-bit. **Best learning value.** |
| L40S | 48 GB | ~$0.79/hr | — | Headroom for longer context / bigger batches |
| A100 80GB | 80 GB | ~$1.19–1.39/hr | ~$1.99/hr | 32B comfortably, tensor-parallel experiments |
| H100 PCIe | 80 GB | ~$1.99/hr | ~$3.29/hr | FP8, max throughput, the "is it worth it?" comparison |

**Mental model for budgeting:**
- A 4090 left running 24/7 ≈ **~$245/month**. That is the thing to avoid.
- The whole point of the cost work is **scale-to-zero** (serverless) + **batching**, so
  you pay for seconds of compute, not idle hours.
- Target a **learning budget of ~$50–150** total: most experiments are minutes-to-hours
  of GPU time, not days.

> **Cost guardrails (build these in week 1):** billing alerts, an auto-shutdown / idle
> timeout on every pod, and a habit of `terminate` not `stop`. The most common way to
> lose money here is a forgotten running GPU.

---

## 5. Phased build plan

Each phase is independently runnable and ends with something you can demo + a short
write-up. This mirrors the rest of this repo (a README + code + tests per topic).

### Phase 0 — Local dry run (cost: $0)
**Goal:** Understand the moving parts before paying for a GPU.
- Run the target model locally via **Ollama** (CPU/small-GPU) to get a feel for it.
- Stand up vLLM in CPU mode or against a tiny model; hit the OpenAI-compatible `/v1`.
- Write a 20-line client that does a chat completion + a streaming completion.
- **Deliverable:** `phase0-local/` with a working `chat.py` and notes on what each
  vLLM flag does.

### Phase 1 — First cloud deployment (cost: ~$1–5)
**Goal:** Get Qwen3-8B serving on a rented GPU.
- Provision an RTX 4090 / L40S on RunPod (pod or template).
- Launch vLLM with the OpenAI server: model, `--max-model-len`, `--gpu-memory-utilization`.
- Confirm a completion from your laptop over the pod's exposed port.
- **Deliverable:** `phase1-deploy/` — a one-command launch script + a teardown script,
  and a README documenting cold-start time and idle cost.

### Phase 2 — Public, secured API (cost: ~$2–10)
**Goal:** A real endpoint you'd be willing to put on the internet.
- Put **Caddy** in front of vLLM: auto-TLS on a domain/subdomain.
- Add **API-key auth** + **rate limiting** at the edge (never expose vLLM directly).
- Handle **streaming (SSE)** correctly through the proxy.
- Threat-model it: what happens under a token-flood / context-bomb attack? Add max
  tokens + concurrency caps.
- **Deliverable:** `phase2-gateway/` — Caddyfile, auth middleware, and a "security
  notes" doc (the enterprise-relevant part).

### Phase 3 — MCP server wrapping the model (cost: minimal)
**Goal:** Connect Claude to your model.
- Build a **FastMCP** server exposing a small, well-designed tool surface, e.g.:
  - `ask_local_model(prompt, max_tokens)` — general completion
  - `summarize(text)` — a constrained, prompt-templated tool
  - `classify(text, labels)` — structured output demo
- Support **stdio** (for Claude Desktop/Code locally) and **streamable HTTP** (for the
  hosted case).
- The server calls your Phase 2 endpoint with the API key held server-side.
- Document the Claude config (`.mcp.json` / Claude Desktop config) to wire it up.
- **Deliverable:** `phase3-mcp/` — the MCP server, config examples, and a recorded
  demo of Claude calling the local model as a tool.

### Phase 4 — Benchmarking harness (cost: ~$5–20)
**Goal:** Measure before you optimize. (Detailed methodology in §6.)
- Define SLOs (e.g. P95 TTFT < 500ms, P95 TPOT < 50ms at N concurrent users).
- Run **GuideLLM** / `vllm bench serve` sweeps across concurrency levels.
- Wire up **Prometheus + Grafana**; capture tokens/s, GPU util, KV-cache usage.
- Produce a baseline **$/million-tokens** for each (GPU, model, config) combo.
- **Deliverable:** `phase4-bench/` — reproducible benchmark scripts, raw results
  (CSV/JSON), and dashboards-as-code.

### Phase 5 — Cost & performance optimization (cost: ~$10–40)
**Goal:** Drive down $/Mtok systematically, one variable at a time. (See §6.4.)
- Quantization sweep (FP16 → AWQ/GPTQ 4-bit → FP8) and measure quality vs cost.
- Batching / concurrency tuning; `--max-num-seqs`, chunked prefill, prefix caching.
- Tensor parallelism for the 32B detour; speculative decoding experiment.
- GPU shootout: 4090 vs L40S vs A100 vs H100 on the *same* workload → cost-efficiency.
- **Deliverable:** `phase5-optimize/` — a before/after table and a written
  "what actually mattered" analysis (the headline artifact of the whole project).

### Phase 6 — Serverless / scale-to-zero & the AWS comparison (cost: ~$5–20)
**Goal:** Make idle cost ≈ $0, then feel the enterprise environment.
- Deploy on **RunPod Serverless** (or Modal): autoscale to zero, measure cold starts,
  and quantify the latency↔cost trade-off of keeping a warm worker.
- Re-deploy the same stack on **AWS** (EC2 GPU + Terraform, or EKS) to experience IAM,
  VPC, security groups, GPU quota requests, and why hyperscaler GPUs cost more.
- **Deliverable:** `phase6-serverless/` + `phase6-aws/` — both deployments plus a
  decision matrix: when GPU-native cloud wins vs when an enterprise picks AWS.

### Phase 7 — Package as a teaching repo (cost: $0)
**Goal:** Turn the work into the open-source learning resource that is the actual goal.
- Top-level README with the architecture, the cost story, and a "start here" path.
- Each phase self-contained and reproducible; one-command up/down everywhere.
- A `COSTS.md` with the real numbers you spent and learned.
- Diagrams, a short walkthrough doc, and clearly stated assumptions.

---

## 6. Benchmarking & cost-optimization methodology (the core of the project)

This is where most of the *learning* lives, so it gets its own detailed treatment.

### 6.1 Metrics that matter
- **TTFT** (Time To First Token) — interactivity; dominated by prefill + queueing.
- **TPOT / ITL** (Time Per Output Token / Inter-Token Latency) — perceived speed.
- **Throughput** — total tokens/s across all concurrent requests (the cost driver).
- **Concurrency at SLO** — max users you can serve while still meeting TTFT/TPOT targets.
- **GPU utilization & KV-cache utilization** — are you compute-bound or memory-bound?
- **$/million tokens** = `GPU $/hr ÷ (throughput tokens/s × 3600 ÷ 1e6)`. The one
  number that ties everything together.

### 6.2 Methodology rules (so results are trustworthy)
- **One variable at a time.** Change quantization OR batch size OR GPU — never two.
- **Fixed, realistic workload.** Define input/output token distributions up front
  (e.g. 512 in / 256 out) and reuse them. Use a dataset like ShareGPT for realism.
- **Warm up** before measuring; discard the first runs.
- **Sweep concurrency** (1, 2, 4, 8, 16, 32, …) to find the throughput knee and the
  point where latency SLOs break.
- **Report percentiles** (P50/P95/P99), never just averages.
- **Pin versions** (vLLM, CUDA, model revision) and record them with every result.

### 6.3 The test matrix
Run the same harness across the cartesian product (subset as budget allows):

```
models      × {Qwen3-8B FP16, Qwen3-8B AWQ-4bit, Qwen3-32B AWQ-4bit}
GPUs        × {4090, L40S, A100-80, H100}
configs     × {batch sizes, --max-model-len, prefix caching on/off, chunked prefill}
concurrency × {1, 4, 8, 16, 32, 64}
```
For each cell record: TTFT P95, TPOT P95, throughput, GPU/KV util, and $/Mtok.

### 6.4 Optimization levers (expected impact, validate empirically)
1. **Continuous batching** (vLLM default) — the single biggest throughput win vs naive.
2. **Quantization** (AWQ/GPTQ 4-bit, FP8 on Hopper) — fit bigger models / bigger
   batches in the same VRAM; usually large $/Mtok wins with small quality loss.
3. **Prefix caching** — big win when prompts share a long system prefix (agents, RAG).
4. **`--max-num-seqs` / `--gpu-memory-utilization`** — tune KV-cache headroom vs OOM risk.
5. **Chunked prefill** — smooths TTFT under mixed long/short requests.
6. **Tensor parallelism** — required for big models; measure the communication overhead.
7. **Speculative decoding** — latency win for the right workloads; measure acceptance rate.
8. **Right-sizing the GPU** — a cheaper GPU at higher utilization often beats an
   expensive one half-idle. This is the punchline of the cost work.
9. **Scale-to-zero** — for bursty/low-traffic, serverless idle savings dwarf everything.

### 6.5 The deliverable artifact
A single results table + narrative answering: *"For workload X at SLO Y, the cheapest
configuration is Z, at $N per million tokens, and here's why."* That sentence, backed by
reproducible data, is the whole project in one line.

---

## 7. Security & operational concerns (the "enterprise" lessons)

- **Never expose the raw vLLM port.** Always behind the auth/TLS edge.
- **Auth + rate limiting + quotas** per key; cap `max_tokens` and concurrency.
- **Secrets management** — API keys and HF tokens via env/secret store, never in git.
- **Cost guardrails** — billing alerts, idle auto-shutdown, spend caps.
- **Abuse / DoS** — context-bomb and token-flood protection at the edge.
- **Observability** — logs, metrics, and a dashboard you'd trust on call.
- **Reproducibility** — pinned images and IaC so a teammate can recreate it cold.
- **Data handling** — be explicit that prompts hit your own infra (a selling point of
  self-hosting) and document any logging of request content.

---

## 8. Proposed repository layout

```
llm-hosting/
├── PLAN.md                 # this file
├── README.md               # start-here overview
├── COSTS.md                # real spend + learnings (filled in as you go)
├── docs/
│   ├── architecture.md
│   ├── benchmarking-methodology.md
│   └── security-notes.md
├── phase0-local/
├── phase1-deploy/
├── phase2-gateway/
├── phase3-mcp/
├── phase4-bench/
├── phase5-optimize/
├── phase6-serverless/
├── phase6-aws/
└── results/                # benchmark CSV/JSON + Grafana dashboards-as-code
```

---

## 9. Suggested timeline (calendar-flexible, ~budget aware)

| Week | Focus | Approx spend |
|---|---|---|
| 1 | Phases 0–1: local dry run + first cloud deploy | $1–5 |
| 2 | Phases 2–3: secured API + MCP, Claude talking to your model | $5–15 |
| 3 | Phase 4: benchmarking harness + dashboards | $5–20 |
| 4 | Phase 5: optimization sweeps, the headline cost analysis | $10–40 |
| 5 | Phase 6–7: serverless + AWS comparison, package as OSS | $10–30 |

**Total learning budget:** comfortably under ~$150, likely less with discipline.

---

## 10. Stretch goals (once the core works)

- **RAG add-on** — vector DB + retrieval tool exposed via MCP.
- **Multi-model routing** — small model for easy queries, big for hard ones.
- **LoRA fine-tune** a small adapter and serve it with vLLM's multi-LoRA support.
- **A/B quality eval** — compare your hosted model vs a frontier API on a task set.
- **Autoscaling under real load** — drive traffic and watch it scale.

---

## 10a. Step-by-step implementation runbook

For a fully prescriptive, copy-paste build guide (exact commands, complete file contents,
and a VERIFY check after every step — written so a small/cheap coding model can execute it
without making decisions), see **[docs/IMPLEMENTATION.md](./docs/IMPLEMENTATION.md)**.

---

## 10b. Production challenges & problem backlog

The real-world failure modes (cold starts, KV-cache limits, OOM crashes, noisy
neighbors, cost-DoS, model lifecycle, reproducibility) and a set of solvable,
measurable **problem statements** for each live in
**[docs/production-challenges.md](./docs/production-challenges.md)**. Pull the relevant
ones into each phase as you build.

---

## 11. Open questions to revisit (not blocking)

- Domain name for the public endpoint (needed for clean TLS in Phase 2).
- Whether to also publish a Llama-3.1-8B variant for license-comparison teaching.
- How much of the AWS phase to do (it is the priciest, least "fun" part — but the most
  enterprise-relevant).

---

## 12. References (verify before spending — prices move fast)

- GPU pricing comparison (2026): https://www.spheron.network/blog/gpu-cloud-pricing-comparison-2026/
- RunPod pricing: https://www.runpod.io/pricing
- Lambda pricing: https://lambda.ai/pricing
- Best open-source LLMs 2026: https://huggingface.co/blog/daya-shankar/open-source-llms
- vLLM docs: https://docs.vllm.ai/
- Model Context Protocol: https://modelcontextprotocol.io/
- FastMCP: https://github.com/jlowin/fastmcp
- GuideLLM (LLM benchmarking): https://github.com/neuralmagic/guidellm
- SkyPilot (multi-cloud): https://skypilot.readthedocs.io/
- Caddy (auto-TLS): https://caddyserver.com/
```
