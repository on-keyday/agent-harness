#!/usr/bin/env python3
"""Probe the undocumented claude stdio control channel.

Answers the three Open questions in
docs/superpowers/specs/2026-08-20-event-stream-agent-design.md:

  1. the `deny` control_response shape
  2. whether hook_callback / mcp_message ever arrive on this channel
  3. whether interrupt_receipt_v1 is usable for cancel

Run one scenario at a time:  probe_control.py deny|hook|interrupt
Everything the child writes is echoed to a transcript file so the raw
envelopes survive, not just my reading of them.
"""
import json, os, subprocess, sys, threading, time, queue, pathlib

HERE = pathlib.Path(__file__).parent
MODEL = "claude-haiku-4-5-20251001"


class Claude:
    def __init__(self, workdir, extra_args=(), settings=None):
        self.workdir = str(workdir)
        argv = [
            "claude", "-p",
            "--input-format", "stream-json",
            "--output-format", "stream-json",
            "--verbose",
            "--model", MODEL,
            "--permission-prompt-tool", "stdio",
            *extra_args,
        ]
        if settings:
            argv += ["--settings", settings]
        self.argv = argv
        env = dict(os.environ)
        # Scrub the harness/agent markers: a nested claude inheriting these
        # loses its local transcript (project_claude_2175_no_local_transcript).
        for k in list(env):
            if k.startswith(("CLAUDE_CODE_", "HARNESS_")) or k in ("CLAUDECODE", "AI_AGENT"):
                env.pop(k, None)
        self.p = subprocess.Popen(
            argv, cwd=self.workdir, env=env,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, bufsize=1,
        )
        self.q = queue.Queue()
        self.raw = []
        threading.Thread(target=self._pump, args=(self.p.stdout, "out"), daemon=True).start()
        threading.Thread(target=self._pump, args=(self.p.stderr, "err"), daemon=True).start()

    def _pump(self, stream, tag):
        for line in stream:
            self.raw.append((tag, line.rstrip("\n")))
            if tag == "out":
                try:
                    self.q.put(json.loads(line))
                except json.JSONDecodeError:
                    self.q.put({"type": "__unparsed__", "line": line.rstrip("\n")})
            else:
                self.q.put({"type": "__stderr__", "line": line.rstrip("\n")})

    def send(self, obj):
        self.p.stdin.write(json.dumps(obj) + "\n")
        self.p.stdin.flush()

    def user_turn(self, text):
        self.send({"type": "user", "message": {"role": "user", "content": text}})

    def until(self, pred, timeout=180):
        """Drain messages until pred(msg) is true; returns (hit, everything)."""
        seen, end = [], time.time() + timeout
        while time.time() < end:
            try:
                m = self.q.get(timeout=1.0)
            except queue.Empty:
                if self.p.poll() is not None and self.q.empty():
                    break
                continue
            seen.append(m)
            if pred(m):
                return m, seen
        return None, seen

    def close(self):
        try:
            self.p.stdin.close()
        except Exception:
            pass
        try:
            self.p.wait(timeout=20)
        except subprocess.TimeoutExpired:
            self.p.kill()

    def dump(self, name):
        path = HERE / f"transcript-{name}.jsonl"
        with open(path, "w") as f:
            for tag, line in self.raw:
                f.write(f"{tag}\t{line}\n")
        return path


def is_can_use_tool(m):
    return (m.get("type") == "control_request"
            and (m.get("request") or {}).get("subtype") == "can_use_tool")


def brief(m, n=320):
    s = json.dumps(m, ensure_ascii=False)
    return s if len(s) <= n else s[:n] + f"…(+{len(s)-n})"


# ---------------------------------------------------------------- scenarios

def scenario_deny(tmp):
    """Open question 1: what does a deny control_response look like, and what
    does the CLI do with it? The spec forbids inferring it from `allow`."""
    c = Claude(tmp)
    c.user_turn("Use the Write tool to create a file named probe.txt containing the single word HELLO. Do not ask me anything first.")
    req, _ = c.until(is_can_use_tool)
    if not req:
        print("NO can_use_tool ARRIVED"); c.close(); print(c.dump("deny")); return
    r = req["request"]
    print("REQUEST:", brief(req))
    print("  keys on request:", sorted(r.keys()))
    print("  permission_suggestions:", json.dumps(r.get("permission_suggestions"), ensure_ascii=False))

    # The shape under test. `message` is the guess that most needs checking:
    # if the CLI ignores it, an operator's deny reason never reaches the agent.
    resp = {
        "type": "control_response",
        "response": {
            "subtype": "success",
            "request_id": req["request_id"],
            "response": {"behavior": "deny", "message": "probe: denied on purpose"},
        },
    }
    print("SENDING:", brief(resp))
    c.send(resp)

    # Did the agent learn WHY, and did it stop or retry?
    end, seen = c.until(lambda m: m.get("type") == "result", timeout=180)
    print("\n--- what followed the deny ---")
    for m in seen:
        t = m.get("type")
        if t in ("assistant", "user"):
            content = (m.get("message") or {}).get("content")
            print(f"  {t}: {brief(content, 400)}")
        elif t == "control_request":
            print(f"  control_request: {brief(m)}")
        elif t in ("result", "__stderr__", "__unparsed__", "system"):
            print(f"  {t}: {brief(m, 300)}")
    print("\nfile created?", (pathlib.Path(tmp) / "probe.txt").exists())
    c.close()
    print("transcript:", c.dump("deny"))


def scenario_hook(tmp):
    """Open question 2: do hook_callback / mcp_message ride this channel?
    A PreToolUse hook is configured via --settings; if hooks are dispatched
    over the control channel, the runner's own settings.json injection would
    interact with the adapter."""
    settings = {
        "hooks": {
            "PreToolUse": [{
                "matcher": "Write",
                "hooks": [{"type": "command", "command": "echo probe-hook-ran >&2"}],
            }]
        }
    }
    sp = pathlib.Path(tmp) / "settings.json"
    sp.write_text(json.dumps(settings))
    c = Claude(tmp, settings=str(sp))
    c.user_turn("Use the Write tool to create hooked.txt containing OK. Do not ask me anything first.")

    subtypes, hook_seen = [], False
    end = time.time() + 180
    while time.time() < end:
        try:
            m = c.q.get(timeout=1.0)
        except queue.Empty:
            if c.p.poll() is not None and c.q.empty():
                break
            continue
        if m.get("type") == "control_request":
            st = (m.get("request") or {}).get("subtype")
            subtypes.append(st)
            print("control_request subtype:", st, "|", brief(m, 260))
            if st == "can_use_tool":
                c.send({"type": "control_response", "response": {
                    "subtype": "success", "request_id": m["request_id"],
                    "response": {"behavior": "allow", "updatedInput": (m["request"] or {}).get("input", {})}}})
        elif m.get("type") == "__stderr__":
            if "probe-hook-ran" in m["line"]:
                hook_seen = True
            print("  stderr:", m["line"][:200])
        elif m.get("type") == "result":
            break
    print("\ncontrol_request subtypes seen:", subtypes)
    print("hook actually ran (stderr marker):", hook_seen)
    print("=> hooks ride the control channel:", "hook_callback" in subtypes)
    c.close()
    print("transcript:", c.dump("hook"))


def scenario_interrupt(tmp):
    """Open question 3: is interrupt_receipt_v1 usable to cancel a turn, or is
    killing the process the only option (what the PTY kind does)?"""
    c = Claude(tmp)
    c.user_turn("Count slowly from 1 to 40, writing a short sentence about each number. Take your time.")
    init, _ = c.until(lambda m: m.get("type") == "system" and m.get("subtype") == "init", timeout=60)
    if init:
        print("init.capabilities:", json.dumps(init.get("capabilities"), ensure_ascii=False))
    # Let it get going, then interrupt.
    time.sleep(6)
    req = {"type": "control_request", "request_id": "probe-int-1",
           "request": {"subtype": "interrupt"}}
    print("SENDING:", brief(req))
    c.send(req)
    got, seen = c.until(lambda m: m.get("type") == "control_response", timeout=60)
    print("control_response:", brief(got) if got else "NONE")
    tail = [m for m in seen if m.get("type") in ("result", "system", "__stderr__", "__unparsed__")]
    for m in tail[-6:]:
        print("  ", brief(m, 260))
    # Is the session still usable after an interrupt? That is the difference
    # between "cancel a turn" and "kill the task".
    print("\nstill alive after interrupt:", c.p.poll() is None)
    if c.p.poll() is None:
        c.user_turn("Reply with exactly: STILL-ALIVE")
        res, seen2 = c.until(lambda m: m.get("type") == "result", timeout=120)
        txt = " ".join(
            b.get("text", "") for m in seen2 if m.get("type") == "assistant"
            for b in ((m.get("message") or {}).get("content") or []) if isinstance(b, dict))
        print("second turn answered:", brief(txt, 200))
    c.close()
    print("transcript:", c.dump("interrupt"))


def scenario_hookblock(tmp):
    """Positive control for scenario_hook. A PreToolUse hook that exits 2 is
    documented to BLOCK the tool, which is observable in the transcript no
    matter where hook stderr goes. If the Write still lands, --settings was
    never honoured and the hook scenario proved nothing."""
    blocker = pathlib.Path(tmp) / "block.sh"
    blocker.write_text("#!/bin/sh\necho 'probe: blocked by PreToolUse hook' >&2\nexit 2\n")
    blocker.chmod(0o755)
    settings = {"hooks": {"PreToolUse": [{"matcher": "Write", "hooks": [
        {"type": "command", "command": str(blocker)}]}]}}
    sp = pathlib.Path(tmp) / "settings.json"
    sp.write_text(json.dumps(settings))
    c = Claude(tmp, settings=str(sp))
    c.user_turn("Use the Write tool to create blocked.txt containing OK. Do not ask me anything first.")
    subtypes = []
    end = time.time() + 180
    while time.time() < end:
        try:
            m = c.q.get(timeout=1.0)
        except queue.Empty:
            if c.p.poll() is not None and c.q.empty():
                break
            continue
        if m.get("type") == "control_request":
            st = (m.get("request") or {}).get("subtype")
            subtypes.append(st)
            print("control_request subtype:", st)
            if st == "can_use_tool":
                c.send({"type": "control_response", "response": {
                    "subtype": "success", "request_id": m["request_id"],
                    "response": {"behavior": "allow", "updatedInput": (m["request"] or {}).get("input", {})}}})
        elif m.get("type") == "user":
            print("  tool_result:", brief((m.get("message") or {}).get("content"), 300))
        elif m.get("type") == "result":
            break
    created = (pathlib.Path(tmp) / "blocked.txt").exists()
    print("\ncontrol_request subtypes:", subtypes)
    print("blocked.txt created:", created)
    print("=> --settings hooks honoured:", not created,
          "(if False, the hook scenario is VACUOUS)")
    c.close()
    print("transcript:", c.dump("hookblock"))


if __name__ == "__main__":
    which = sys.argv[1] if len(sys.argv) > 1 else "deny"
    tmp = HERE / f"work-{which}"
    tmp.mkdir(exist_ok=True)
    {"deny": scenario_deny, "hook": scenario_hook, "interrupt": scenario_interrupt,
     "hookblock": scenario_hookblock}[which](tmp)
