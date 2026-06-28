# LLM Hosting — Self-Host an Open-Source Model End-to-End

An open-source, learn-by-building project for hosting an open-weight LLM on cloud GPU
infrastructure, exposing it as a secured public API, connecting it to **Claude via MCP**,
and **benchmarking + cost-optimizing** the inference stack.

The goal is to teach the *real-life complexities* of running models for an enterprise —
not a toy demo.

## Start here

📋 **[PLAN.md](./PLAN.md)** — the full master plan: architecture, phased build steps,
benchmarking methodology, cost model, and security concerns.

## The one-line objective

> *"For a given workload and latency SLO, find the cheapest model + GPU + serving
> configuration, expressed as $ per million tokens — and be able to explain why."*

## Stack at a glance

- **Model:** Qwen3-8B (Apache 2.0), with a 32B quantized detour
- **Serving:** vLLM (OpenAI-compatible, continuous batching, PagedAttention)
- **Cloud:** RunPod (GPU-native, cheap) first; AWS as the enterprise comparison
- **Edge:** Caddy (auto-TLS) + API-key auth + rate limiting
- **Claude integration:** a FastMCP server wrapping the model endpoint
- **Benchmarking:** GuideLLM / `vllm bench serve` + Prometheus + Grafana

## Phases

| Phase | What | Status |
|---|---|---|
| 0 | Local dry run | 🔲 planned |
| 1 | First cloud deployment (vLLM on a GPU) | 🔲 planned |
| 2 | Secured public API (TLS, auth, rate limit) | 🔲 planned |
| 3 | MCP server → Claude talks to the model | 🔲 planned |
| 4 | Benchmarking harness + dashboards | 🔲 planned |
| 5 | Cost & performance optimization | 🔲 planned |
| 6 | Serverless scale-to-zero + AWS comparison | 🔲 planned |
| 7 | Package as a teaching repo | 🔲 planned |

See [PLAN.md](./PLAN.md) for details on each phase.
