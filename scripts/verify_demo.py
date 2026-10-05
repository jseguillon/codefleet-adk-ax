#!/usr/bin/env python3
"""End-to-end evidence assertions; no cluster, provider or API key required."""
import json
import os
from pathlib import Path
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[1]
BIN = ROOT / "bin/codefleet"

def run(label, flags, env=None, success=True):
    proc = subprocess.run([str(BIN), "demo", "--exit", "--listen", "127.0.0.1:0", "--out", str(ROOT / "runs" / label), *flags], cwd=ROOT, env=env, capture_output=True, text=True, timeout=90)
    if (proc.returncode == 0) != success:
        raise AssertionError(proc.stdout + proc.stderr)
    dirs = list((ROOT / "runs" / label).glob("cf-*"))
    report_path = max(dirs, key=lambda p: p.stat().st_mtime) / "report.json"
    report = json.loads(report_path.read_text())
    assert report["status"] == ("completed" if success else "failed"), report
    print(f"{label}: {report['status']} ({len(report['results'])} AX tasks)")
    return report, report_path.parent

def by_node(report):
    return {r["node"]: r for r in report["results"].values()}

def assert_order(report, first, second):
    events = report["events"]
    end = next(e["seq"] for e in events if e["node"] == first and e["kind"] == "result")
    start = next(e["seq"] for e in events if e["node"] == second and e["kind"] == "creating")
    assert end < start, (first, second)

def main():
    repaired, directory = run("repair-and-resume", ["--delay-ms", "400"])
    nodes = by_node(repaired)
    assert len(nodes) == 14
    assert nodes["backend"]["reused_checkpoint"]
    pids = {e['data']['pid'] for e in repaired['events'] if e['node'] == 'backend' and e['kind'] == 'worker_ready'}
    assert len(pids) == 2, 'resume must start a different process'
    assert any(e["kind"] == "retry" for e in repaired["events"])
    assert any(e["kind"] == "suspended" for e in repaired["events"])
    assert not all(c["passed"] for c in nodes["unit"]["checks"])
    assert all(c["passed"] for c in nodes["review_final"]["checks"])
    assert all(r["llm_calls"] == 0 for r in nodes.values())
    assert_order(repaired, "docs", "integrate")
    assert_order(repaired, "frontend", "integrate")
    assert_order(repaired, "backend", "integrate")
    assert_order(repaired, "unit", "review")
    assert_order(repaired, "security", "review")
    # All writer tasks start before any finishes: real fan-out, not a serial log.
    events = repaired["events"]
    starts = [e["seq"] for e in events if e["node"] in ("backend", "frontend", "docs") and e["kind"] == "worker_ready"]
    finishes = [e["seq"] for e in events if e["node"] in ("backend", "frontend", "docs") and e["kind"] == "result"]
    assert max(starts) < min(finishes)
    for definition in repaired["nodes"]:
        path = directory / "actors" / (repaired["run_id"] + "-" + definition["id"])
        mounted = [p.name for p in path.iterdir() if p.is_dir() and p.name != "skills"]
        assert sorted(mounted) == sorted(definition["repos"] or []), (definition["id"], mounted)
        assert sorted(p.name for p in (path / "skills").iterdir()) == [definition["skill"] + ".md"]
    for repo, command in [("api", ["python3", "-m", "unittest", "-v"]), ("web", ["node", "--test", "coupon.test.mjs"])]:
        subprocess.run(command, cwd=directory / "bundle" / repo, check=True, capture_output=True)
    assert 'contract.json' in (directory / 'api.patch').read_text()
    happy, _ = run("happy-path", ["--cycle=false", "--inject-bug=false", "--delay-ms", "0"])
    assert len(happy["results"]) == 10 and "repair" not in by_node(happy)
    cancelled, _ = run("timeout", ["--timeout", "100ms", "--delay-ms", "400"], success=False)
    assert "handoff" not in by_node(cancelled)
    # A mock OpenAI-compatible provider exercises the CLI/API path, not inference.
    class Handler(BaseHTTPRequestHandler):
        bad = False
        def log_message(self, *_): pass
        def do_POST(self):
            assert self.path == "/v1/chat/completions"
            assert self.headers.get("Authorization") == "Bearer test-key"
            req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            prompt = req["messages"][-1]["content"]
            if "Role: backend" in prompt or "Role: repair" in prompt:
                content = "def apply_coupon(total_cents, percent):\n    if type(total_cents) is not int or type(percent) is not int or total_cents < 0 or not 0 <= percent <= 100:\n        raise ValueError('invalid')\n    return total_cents * (100 - percent) // 100\n"
            elif "Role: frontend" in prompt:
                content = "export function previewCoupon(totalCents, percent) {\n  if (!Number.isSafeInteger(totalCents) || !Number.isSafeInteger(percent) || totalCents < 0 || percent < 0 || percent > 100) throw new Error('invalid');\n  return Math.floor(totalCents * (100 - percent) / 100);\n}\n"
            else:
                content = "# Coupons\n999 cents with 15% gives 849 cents.\n"
            text = "broken JSON" if self.bad else json.dumps({"content": content})
            payload = json.dumps({"choices": [{"message": {"content": text}}]}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers(); self.wfile.write(payload)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
    env = dict(os.environ, OPENAI_BASE_URL=f"http://127.0.0.1:{server.server_port}/v1", OPENAI_MODEL="mock-provider", OPENAI_API_KEY="test-key")
    try:
        llm, _ = run("compatible-api-mock", ["--mode", "llm", "--cycle=false", "--delay-ms", "0"], env)
        assert sum(r["llm_calls"] for r in llm["results"].values()) == 3
        assert "test-key" not in json.dumps(llm)
        Handler.bad = True
        failed, _ = run("invalid-provider-json", ["--mode", "llm", "--cycle=false", "--delay-ms", "0"], env, success=False)
        assert "handoff" not in by_node(failed)
    finally:
        server.shutdown(); server.server_close()
    print("PASS: fan-out/fan-in, scopes, CLI tasks, retry, process restart, immutable artifacts, repair, fresh gates, timeout, compatible API and invalid provider output")

if __name__ == "__main__": main()
