#!/usr/bin/env python3
"""How does an event-stream session actually END?

Two candidates, both so far taken on faith:

  stdin-close : claimed to let the in-flight turn finish
  sigterm     : documented to abort the turn and run SessionEnd hooks

Measured per case: does the turn complete, what is the exit code, and does a
configured SessionEnd hook fire? That last one is the whole decision — if
stdin-close also runs SessionEnd there is no tradeoff to make.

  probe_finish.py stdin-close | sigterm | stdin-close-idle
"""
import json, os, signal, subprocess, sys, threading, time, queue, pathlib

HERE = pathlib.Path(__file__).parent
MODEL = "claude-haiku-4-5-20251001"


def start(work, marker):
    settings = {"hooks": {"SessionEnd": [{"hooks": [
        {"type": "command", "command": f"touch {marker}"}]}]}}
    sp = work / "settings.json"
    sp.write_text(json.dumps(settings))
    env = dict(os.environ)
    for k in list(env):
        if k.startswith(("CLAUDE_CODE_", "HARNESS_")) or k in ("CLAUDECODE", "AI_AGENT"):
            env.pop(k, None)
    p = subprocess.Popen(
        ["claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json",
         "--verbose", "--model", MODEL, "--settings", str(sp)],
        cwd=str(work), env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, text=True, bufsize=1)
    q = queue.Queue()
    threading.Thread(target=lambda: [q.put(("o", l)) for l in p.stdout], daemon=True).start()
    threading.Thread(target=lambda: [q.put(("e", l)) for l in p.stderr], daemon=True).start()
    return p, q


def send(p, o):
    p.stdin.write(json.dumps(o) + "\n")
    p.stdin.flush()


def drain(p, q, seconds, sink):
    end = time.time() + seconds
    while time.time() < end:
        try:
            tag, line = q.get(timeout=0.4)
        except queue.Empty:
            if p.poll() is not None:
                return
            continue
        sink.append((tag, line.rstrip()))


def summarise(lines):
    kinds, texts, results = [], [], []
    for tag, l in lines:
        if tag != "o":
            continue
        try:
            m = json.loads(l)
        except Exception:
            continue
        t = m.get("type")
        if t == "system" and m.get("subtype") == "thinking_tokens":
            continue
        kinds.append(f'{t}/{m.get("subtype","")}'.rstrip("/"))
        if t == "assistant":
            for b in (m.get("message") or {}).get("content") or []:
                if isinstance(b, dict) and b.get("type") == "text":
                    texts.append(b["text"])
        if t == "result":
            results.append({"subtype": m.get("subtype"), "is_error": m.get("is_error"),
                            "num_turns": m.get("num_turns")})
    return kinds, texts, results


def report(name, p, lines, marker, elapsed):
    kinds, texts, results = summarise(lines)
    print(f"\n=== {name}")
    print(f"  exit code        : {p.returncode}")
    print(f"  seconds to exit  : {elapsed:.1f}")
    print(f"  result messages  : {results if results else 'NONE (turn never completed)'}")
    print(f"  SessionEnd hook  : {'FIRED' if marker.exists() else 'did not fire'}")
    body = " ".join(texts)
    print(f"  assistant text   : {len(body)} chars | {body[:80]!r}")
    print(f"  last kinds       : {kinds[-6:]}")


def case_stdin_close(work, marker):
    """Close stdin WHILE a turn is running."""
    p, q = start(work, marker)
    lines = []
    drain(p, q, 8, lines)  # let init land
    send(p, {"type": "user", "message": {"role": "user",
             "content": "Count from 1 to 30, writing one short sentence about each number."}})
    drain(p, q, 6, lines)  # let the turn get going
    t0 = time.time()
    p.stdin.close()
    try:
        p.wait(timeout=180)
    except subprocess.TimeoutExpired:
        p.kill()
    drain(p, q, 3, lines)
    report("stdin close, mid-turn", p, lines, marker, time.time() - t0)


def case_stdin_close_idle(work, marker):
    """Close stdin with NO turn in flight."""
    p, q = start(work, marker)
    lines = []
    drain(p, q, 8, lines)
    send(p, {"type": "user", "message": {"role": "user", "content": "Reply with just: OK"}})
    end = time.time() + 90
    while time.time() < end:
        drain(p, q, 1, lines)
        if any('"type":"result"' in l for _, l in lines):
            break
    t0 = time.time()
    p.stdin.close()
    try:
        p.wait(timeout=120)
    except subprocess.TimeoutExpired:
        p.kill()
    drain(p, q, 3, lines)
    report("stdin close, idle", p, lines, marker, time.time() - t0)


def case_sigterm(work, marker):
    """SIGTERM while a turn is running."""
    p, q = start(work, marker)
    lines = []
    drain(p, q, 8, lines)
    send(p, {"type": "user", "message": {"role": "user",
             "content": "Count from 1 to 30, writing one short sentence about each number."}})
    drain(p, q, 6, lines)
    t0 = time.time()
    p.send_signal(signal.SIGTERM)
    try:
        p.wait(timeout=120)
    except subprocess.TimeoutExpired:
        p.kill()
    drain(p, q, 3, lines)
    report("SIGTERM, mid-turn", p, lines, marker, time.time() - t0)


if __name__ == "__main__":
    which = sys.argv[1]
    work = HERE / f"work-finish-{which}"
    work.mkdir(exist_ok=True)
    marker = work / "sessionend.marker"
    if marker.exists():
        marker.unlink()
    {"stdin-close": case_stdin_close, "sigterm": case_sigterm,
     "stdin-close-idle": case_stdin_close_idle}[which](work, marker)
