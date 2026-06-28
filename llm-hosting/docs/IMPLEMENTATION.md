# Implementation Runbook (optimized for a small/cheap coding model)

> **How to use this file.** Execute the steps **in order**. Each step has: a goal, the
> **exact** command(s) or **complete** file contents to create, and a **VERIFY** check.
> Do **not** improvise, do **not** skip VERIFY. If a VERIFY fails, STOP and report the
> exact error — do not continue.
>
> **Rules for the executing model:**
> 1. Run commands exactly as written. Do not add flags.
> 2. When asked to create a file, write the file **verbatim** (full contents shown).
> 3. Replace only the clearly marked `<<PLACEHOLDER>>` tokens.
> 4. After each `VERIFY`, confirm the expected output appears before moving on.
> 5. Never commit secrets. Secrets go in `.env` files that are git-ignored.
>
> **Placeholders you will set once (write them down):**
> - `<<RUNPOD_POD_IP>>` and `<<RUNPOD_VLLM_PORT>>` — from the RunPod pod you create
> - `<<PUBLIC_DOMAIN>>` — a domain/subdomain you control (for TLS), e.g. `llm.example.com`
> - `<<API_KEY>>` — a long random string you generate for auth
> - `<<HF_TOKEN>>` — a Hugging Face read token (only if a model needs it; Qwen3 is open)

---

## Phase 0 — Local dry run ($0, no GPU)

**Goal:** Prove you can talk to an OpenAI-compatible LLM endpoint locally before paying.

### Step 0.1 — Create the project skeleton
```bash
cd llm-hosting
mkdir -p phase0-local phase1-deploy phase2-gateway phase3-mcp phase4-bench phase5-optimize results
```
**VERIFY:** `ls llm-hosting` shows all the directories above.

### Step 0.2 — Install Python tooling (CPU only)
```bash
python3 -m venv phase0-local/.venv
phase0-local/.venv/bin/pip install --upgrade pip
phase0-local/.venv/bin/pip install "openai>=1.0" requests
```
**VERIFY:** `phase0-local/.venv/bin/python -c "import openai; print(openai.__version__)"` prints a version ≥ 1.0.

### Step 0.3 — Run a tiny model locally with Ollama (no GPU needed)
```bash
curl -fsSL https://ollama.com/install.sh | sh
ollama pull qwen2.5:0.5b
ollama serve >/tmp/ollama.log 2>&1 &
sleep 5
```
**VERIFY:** `curl -s http://localhost:11434/api/tags` returns JSON listing `qwen2.5:0.5b`.

### Step 0.4 — Create the test client
Create file `phase0-local/chat.py` with these **exact** contents:
```python
import os
from openai import OpenAI

# Ollama exposes an OpenAI-compatible endpoint at /v1
client = OpenAI(
    base_url=os.environ.get("LLM_BASE_URL", "http://localhost:11434/v1"),
    api_key=os.environ.get("LLM_API_KEY", "ollama"),  # Ollama ignores the key
)
MODEL = os.environ.get("LLM_MODEL", "qwen2.5:0.5b")

def main():
    print("=== non-streaming ===")
    r = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "Say hello in one short sentence."}],
        max_tokens=64,
    )
    print(r.choices[0].message.content)

    print("\n=== streaming ===")
    stream = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "Count from 1 to 5."}],
        max_tokens=64,
        stream=True,
    )
    for chunk in stream:
        delta = chunk.choices[0].delta.content or ""
        print(delta, end="", flush=True)
    print()

if __name__ == "__main__":
    main()
```
**VERIFY:** `phase0-local/.venv/bin/python phase0-local/chat.py` prints a greeting, then a streamed "1 2 3 4 5". If so, Phase 0 is done.

---

## Phase 1 — Deploy Qwen3-8B on a cloud GPU (~$1–5)

**Goal:** A real vLLM server running on a rented GPU, reachable over the network.

### Step 1.1 — Create a RunPod GPU pod (manual, one-time, in the browser)
Do this in the RunPod web console (no CLI required):
1. Sign up at runpod.io and add ~$10 of credit.
2. **Deploy → Pods → GPU.** Pick an **RTX 4090 (24GB)**.
3. Template: choose the official **vLLM** template if present, otherwise **"RunPod PyTorch"**.
4. Set **Container Disk** ≥ 30 GB and **Volume Disk** ≥ 30 GB (for model weights).
5. Under **Expose HTTP Ports**, add port **8000**.
6. Deploy. Wait until status = **Running**, then open **Connect** and note:
   - the pod's public IP or proxy URL → write down as `<<RUNPOD_POD_IP>>`
   - the mapped port for 8000 → write down as `<<RUNPOD_VLLM_PORT>>`

**VERIFY:** The pod shows **Running** and you have a web terminal / SSH into it.

### Step 1.2 — Launch vLLM inside the pod
Open the pod's web terminal (or SSH) and run **inside the pod**:
```bash
pip install "vllm>=0.6.0"
python -m vllm.entrypoints.openai.api_server \
  --model Qwen/Qwen3-8B \
  --host 0.0.0.0 \
  --port 8000 \
  --max-model-len 8192 \
  --gpu-memory-utilization 0.90 \
  --api-key <<API_KEY>>
```
Leave this running. First start downloads weights (a few minutes).

> If `Qwen/Qwen3-8B` fails to resolve, substitute `Qwen/Qwen2.5-7B-Instruct` (same flags).

**VERIFY (inside the pod, new terminal):**
```bash
curl -s http://localhost:8000/v1/models -H "Authorization: Bearer <<API_KEY>>"
```
returns JSON listing the model id.

### Step 1.3 — Reach it from your laptop
On your **local** machine:
```bash
curl -s http://<<RUNPOD_POD_IP>>:<<RUNPOD_VLLM_PORT>>/v1/models \
  -H "Authorization: Bearer <<API_KEY>>"
```
**VERIFY:** returns the same model JSON. If it hangs, the port isn't exposed — re-check Step 1.1.5.

### Step 1.4 — Record cost/teardown notes
Create file `phase1-deploy/README.md`:
```markdown
# Phase 1 — Deploy

- GPU: RTX 4090 (24GB), RunPod
- Model: Qwen/Qwen3-8B via vLLM OpenAI server, port 8000
- Cold start (first weights download): ____ min   (FILL IN)
- Steady idle cost: ~$0.34/hr  → TERMINATE the pod when not in use.

## Teardown (DO THIS to stop billing)
RunPod console → Pods → select pod → **Terminate** (not Stop; Stop still bills storage).
```
**VERIFY:** file exists. **IMPORTANT:** when you finish a work session, **Terminate** the pod.

---

## Phase 2 — Secured public API (TLS + auth + rate limit) (~$2–10)

**Goal:** Put a hardened edge in front of vLLM so the raw port is never public.

> Run the gateway on a **small, cheap CPU box** (a $5/mo VPS or a separate RunPod CPU
> pod), NOT on the GPU. It proxies to the GPU pod. This keeps the GPU port private.

### Step 2.1 — Point your domain at the gateway box
In your DNS provider, create an **A record**: `<<PUBLIC_DOMAIN>>` → the gateway box's IP.
**VERIFY:** `dig +short <<PUBLIC_DOMAIN>>` returns the gateway IP (may take a few minutes).

### Step 2.2 — Install Caddy on the gateway box
```bash
sudo apt-get update && sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt-get update && sudo apt-get install -y caddy
```
**VERIFY:** `caddy version` prints a version.

### Step 2.3 — Write the Caddyfile
Create `/etc/caddy/Caddyfile` with these **exact** contents (replace placeholders):
```
<<PUBLIC_DOMAIN>> {
	# Caddy gets a free Let's Encrypt TLS cert automatically.

	# Simple shared-secret auth: require a matching header.
	@noauth not header Authorization "Bearer <<API_KEY>>"
	respond @noauth "Unauthorized" 401

	# Basic rate limit via request body size cap (blocks giant context bombs).
	request_body {
		max_size 256KB
	}

	reverse_proxy http://<<RUNPOD_POD_IP>>:<<RUNPOD_VLLM_PORT>> {
		# Preserve streaming (SSE) for token-by-token responses.
		flush_interval -1
	}
}
```
Then:
```bash
sudo systemctl restart caddy
```
**VERIFY:**
```bash
# Should be 401 (no/incorrect key):
curl -s -o /dev/null -w "%{http_code}\n" https://<<PUBLIC_DOMAIN>>/v1/models
# Should be 200 with model JSON (correct key):
curl -s https://<<PUBLIC_DOMAIN>>/v1/models -H "Authorization: Bearer <<API_KEY>>"
```
First prints `401`, second prints model JSON over **https**.

### Step 2.4 — Confirm the raw GPU port is NOT publicly reachable
On the GPU pod, restrict inbound so only the gateway box can reach port 8000 (RunPod:
remove the public HTTP expose for 8000 after the gateway works, or use a firewall rule).
**VERIFY:** from your laptop, `curl http://<<RUNPOD_POD_IP>>:<<RUNPOD_VLLM_PORT>>/v1/models`
now **fails/times out**, while the `https://<<PUBLIC_DOMAIN>>` route still works.

### Step 2.5 — Document security posture
Create `phase2-gateway/security-notes.md` noting: TLS via Caddy, bearer-token auth,
256KB body cap, GPU port private. List remaining gaps (token-rate limiting → Phase later).
**VERIFY:** file exists.

---

## Phase 3 — MCP server so Claude can use the model (~$0)

**Goal:** A FastMCP server exposing tools that call your secured endpoint; wire it to Claude.

### Step 3.1 — Set up the MCP project
```bash
python3 -m venv phase3-mcp/.venv
phase3-mcp/.venv/bin/pip install --upgrade pip
phase3-mcp/.venv/bin/pip install "fastmcp>=2.0" "openai>=1.0"
```
**VERIFY:** `phase3-mcp/.venv/bin/python -c "import fastmcp; print('ok')"` prints `ok`.

### Step 3.2 — Create the secrets file (git-ignored)
Create `phase3-mcp/.env`:
```
LLM_BASE_URL=https://<<PUBLIC_DOMAIN>>/v1
LLM_API_KEY=<<API_KEY>>
LLM_MODEL=Qwen/Qwen3-8B
```
Create/append `llm-hosting/.gitignore`:
```
**/.venv/
**/.env
results/*.tmp
```
**VERIFY:** `git status` does NOT list `phase3-mcp/.env`.

### Step 3.3 — Write the MCP server
Create `phase3-mcp/server.py` with these **exact** contents:
```python
import os
from fastmcp import FastMCP
from openai import OpenAI

BASE_URL = os.environ["LLM_BASE_URL"]
API_KEY = os.environ["LLM_API_KEY"]
MODEL = os.environ.get("LLM_MODEL", "Qwen/Qwen3-8B")

client = OpenAI(base_url=BASE_URL, api_key=API_KEY)
mcp = FastMCP("self-hosted-llm")

def _complete(prompt: str, max_tokens: int = 512) -> str:
    r = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": prompt}],
        max_tokens=max_tokens,
    )
    return r.choices[0].message.content or ""

@mcp.tool
def ask_local_model(prompt: str, max_tokens: int = 512) -> str:
    """Send a prompt to the self-hosted open-source model and return its answer."""
    return _complete(prompt, max_tokens)

@mcp.tool
def summarize(text: str) -> str:
    """Summarize the given text in 3 concise bullet points using the local model."""
    return _complete(f"Summarize the following in 3 short bullet points:\n\n{text}", 256)

@mcp.tool
def classify(text: str, labels: list[str]) -> str:
    """Classify text into exactly one of the provided labels. Returns the chosen label."""
    joined = ", ".join(labels)
    return _complete(
        f"Classify the text into exactly one of these labels [{joined}]. "
        f"Reply with only the label.\n\nText: {text}",
        16,
    )

if __name__ == "__main__":
    mcp.run()  # stdio transport by default
```
**VERIFY (loads without error):**
```bash
set -a; source phase3-mcp/.env; set +a
phase3-mcp/.venv/bin/python -c "import server" 2>/dev/null && echo OK || (cd phase3-mcp && ../phase3-mcp/.venv/bin/python -c "import server" && echo OK)
```
Prints `OK` (no traceback).

### Step 3.4 — Smoke-test the underlying endpoint the tools use
```bash
set -a; source phase3-mcp/.env; set +a
phase3-mcp/.venv/bin/python - <<'PY'
import os
from openai import OpenAI
c = OpenAI(base_url=os.environ["LLM_BASE_URL"], api_key=os.environ["LLM_API_KEY"])
r = c.chat.completions.create(model=os.environ["LLM_MODEL"],
    messages=[{"role":"user","content":"Reply with the single word: ready"}], max_tokens=8)
print(r.choices[0].message.content)
PY
```
**VERIFY:** prints something containing `ready`.

### Step 3.5 — Connect Claude to the MCP server
For **Claude Desktop**, edit its config file
(`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS;
`%APPDATA%\Claude\claude_desktop_config.json` on Windows) to include:
```json
{
  "mcpServers": {
    "self-hosted-llm": {
      "command": "<<ABSOLUTE_PATH>>/phase3-mcp/.venv/bin/python",
      "args": ["<<ABSOLUTE_PATH>>/phase3-mcp/server.py"],
      "env": {
        "LLM_BASE_URL": "https://<<PUBLIC_DOMAIN>>/v1",
        "LLM_API_KEY": "<<API_KEY>>",
        "LLM_MODEL": "Qwen/Qwen3-8B"
      }
    }
  }
}
```
Replace `<<ABSOLUTE_PATH>>` with the absolute path to the repo. Restart Claude Desktop.
**VERIFY:** In Claude, the `self-hosted-llm` tools appear; ask Claude to "use ask_local_model
to say hello" and it returns a response from your model.

---

## Phase 4 — Benchmark harness (~$5–20)

**Goal:** Measure TTFT, throughput, and $/Mtok with a repeatable script.

### Step 4.1 — Install the benchmark tool
```bash
python3 -m venv phase4-bench/.venv
phase4-bench/.venv/bin/pip install --upgrade pip
phase4-bench/.venv/bin/pip install guidellm
```
**VERIFY:** `phase4-bench/.venv/bin/guidellm --help` prints usage.

### Step 4.2 — Run a concurrency sweep
Create `phase4-bench/run_bench.sh`:
```bash
#!/usr/bin/env bash
set -euo pipefail
: "${TARGET:?set TARGET=https://<<PUBLIC_DOMAIN>>/v1}"
: "${API_KEY:?set API_KEY=<<API_KEY>>}"
: "${MODEL:=Qwen/Qwen3-8B}"
mkdir -p ../results
for C in 1 4 8 16 32; do
  echo "=== concurrency=$C ==="
  GUIDELLM__OPENAI__API_KEY="$API_KEY" \
  guidellm benchmark \
    --target "$TARGET" \
    --model "$MODEL" \
    --rate-type concurrent \
    --rate "$C" \
    --max-requests 100 \
    --data '{"prompt_tokens":512,"output_tokens":256}' \
    --output-path "../results/bench_c${C}.json"
done
echo "Done. Results in ../results/"
```
Run it:
```bash
chmod +x phase4-bench/run_bench.sh
cd phase4-bench
TARGET=https://<<PUBLIC_DOMAIN>>/v1 API_KEY=<<API_KEY>> ./run_bench.sh
cd ..
```
**VERIFY:** `ls results/bench_c*.json` lists 5 files, each non-empty.

### Step 4.3 — Compute $/million-tokens
Create `phase4-bench/cost.py`:
```python
import json, glob, sys

GPU_COST_PER_HR = float(sys.argv[1]) if len(sys.argv) > 1 else 0.34  # RTX 4090 default

print(f"{'concurrency':>11} | {'tok/s':>10} | {'$/Mtok':>10}")
print("-" * 38)
for path in sorted(glob.glob("../results/bench_c*.json")):
    data = json.load(open(path))
    # GuideLLM stores summary metrics; pull total output throughput (tokens/sec).
    # Adjust key path if the schema differs; print raw keys to inspect.
    try:
        tps = data["benchmarks"][0]["metrics"]["output_tokens_per_second"]["mean"]
    except Exception:
        print(f"{path}: could not find throughput; keys = {list(data.keys())}")
        continue
    cost_per_mtok = GPU_COST_PER_HR / (tps * 3600 / 1e6) if tps else float("inf")
    c = path.split("_c")[1].split(".")[0]
    print(f"{c:>11} | {tps:>10.1f} | {cost_per_mtok:>10.2f}")
```
Run:
```bash
cd phase4-bench && .venv/bin/python cost.py 0.34 ; cd ..
```
**VERIFY:** prints a table of concurrency → tokens/s → $/Mtok. (If the throughput key
isn't found, the script prints the available keys — adjust the key path and rerun.)

### Step 4.4 — Record the baseline
Create `results/BASELINE.md` and paste the table from Step 4.3 plus: GPU, model,
vLLM version, and the input/output token sizes used.
**VERIFY:** file exists with real numbers.

---

## Phase 5 — Optimization (~$10–40)

**Goal:** Lower $/Mtok by changing **one variable at a time** and re-running Phase 4.

### Step 5.1 — Quantization run
Relaunch vLLM in the pod with a 4-bit model, **same flags otherwise**:
```bash
python -m vllm.entrypoints.openai.api_server \
  --model Qwen/Qwen3-8B-AWQ \
  --quantization awq \
  --host 0.0.0.0 --port 8000 \
  --max-model-len 8192 --gpu-memory-utilization 0.90 \
  --api-key <<API_KEY>>
```
> If `Qwen/Qwen3-8B-AWQ` is unavailable, use any official AWQ build of the same family.

Re-run Phase 4 Step 4.2–4.3. Save results as `results/bench_awq_c*.json` (rename the
output path in the script).
**VERIFY:** you have a second table; compare $/Mtok vs baseline.

### Step 5.2 — Prefix-caching run
Relaunch vLLM adding `--enable-prefix-caching`. Re-run the benchmark using a shared long
system prompt. **VERIFY:** TTFT improves for the shared-prefix workload.

### Step 5.3 — Concurrency/KV tuning
Sweep `--max-num-seqs` (e.g. 64, 128, 256) and re-run. **VERIFY:** record the value that
maximizes throughput without OOM.

### Step 5.4 — Write the headline analysis
Create `phase5-optimize/RESULTS.md` with a before/after table (baseline vs each change)
and one sentence: *"For 512in/256out at P95 TTFT < Xms, the cheapest config is ___ at
$___/Mtok."*
**VERIFY:** file exists with a concrete $/Mtok winner.

---

## Phase 6 — Scale-to-zero (serverless) (~$5–20)

**Goal:** Make idle cost ≈ $0.

### Step 6.1 — Deploy on RunPod Serverless (browser, one-time)
RunPod console → **Serverless → New Endpoint** → select a vLLM worker → set model
`Qwen/Qwen3-8B` → min workers `0`, max workers `1` → deploy. Note the endpoint URL.
**VERIFY:** a test request to the endpoint returns a completion (first call is a cold start).

### Step 6.2 — Measure cold start
Send one request after ≥5 min idle; time it. Send a second immediately; time it.
Record both in `phase6-serverless/README.md`.
**VERIFY:** file shows cold vs warm latency numbers.

---

## Final — Commit everything (no secrets)

```bash
cd llm-hosting
# Confirm no secrets staged:
git status
# .env files must NOT appear. If they do, STOP and fix .gitignore.
git add .
git commit -m "Implement self-hosted LLM stack: deploy, gateway, MCP, benchmarks"
```
**VERIFY:** `git status` shows a clean tree and no `.env` files were committed.

---

## Quick failure playbook (for the executing model)

| Symptom | Likely cause | Action |
|---|---|---|
| `curl` to GPU port hangs | port not exposed / firewalled | re-check Phase 1 Step 1.1.5 |
| 401 from `<<PUBLIC_DOMAIN>>` | wrong/missing bearer key | check `Authorization` header matches `<<API_KEY>>` |
| vLLM OOM at startup | model too big / util too high | lower `--gpu-memory-utilization` to 0.85, lower `--max-model-len` |
| TLS cert fails in Caddy | DNS not pointing at box yet | wait for `dig` to resolve, restart caddy |
| Claude doesn't see tools | bad config path | use **absolute** paths in `claude_desktop_config.json`, restart Claude |
| GuideLLM key not found | schema differs | run the cost.py branch that prints available keys, adjust key path |

> When in doubt: STOP, report the exact command and its full output, and ask before
> changing the architecture. Do not invent new steps.
```
