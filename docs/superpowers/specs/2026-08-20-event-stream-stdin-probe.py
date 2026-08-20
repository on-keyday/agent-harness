#!/usr/bin/env python3
"""What does stdin actually need?

Two things travel on the child's stdin -- user turns and control responses --
so the question is which of them forces --input-format stream-json.

  plaintext : plain stdin, no --input-format. Is it one prompt or many turns?
  control   : --permission-prompt-tool stdio WITHOUT --input-format. Does a
              can_use_tool still arrive, and can it be answered on a stdin
              that has no framing?
  both      : stream-json input, interleaving a control response and a user
              turn on the same pipe.
"""
import json, os, subprocess, sys, threading, time, queue, pathlib

HERE = pathlib.Path(__file__).parent
MODEL = "claude-haiku-4-5-20251001"


def spawn(workdir, argv_extra, stdin_pipe=True):
    argv = ["claude", "-p", "--output-format", "stream-json", "--verbose",
            "--model", MODEL, *argv_extra]
    env = dict(os.environ)
    for k in list(env):
        if k.startswith(("CLAUDE_CODE_", "HARNESS_")) or k in ("CLAUDECODE", "AI_AGENT"):
            env.pop(k, None)
    p = subprocess.Popen(argv, cwd=str(workdir), env=env,
                         stdin=subprocess.PIPE if stdin_pipe else subprocess.DEVNULL,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         text=True, bufsize=1)
    q, raw = queue.Queue(), []

    def pump(stream, tag):
        for line in stream:
            raw.append((tag, line.rstrip("\n")))
            if tag == "out":
                try:
                    q.put(json.loads(line))
                except json.JSONDecodeError:
                    q.put({"type": "__unparsed__", "line": line.rstrip("\n")})
            else:
                q.put({"type": "__stderr__", "line": line.rstrip("\n")})
    threading.Thread(target=pump, args=(p.stdout, "out"), daemon=True).start()
    threading.Thread(target=pump, args=(p.stderr, "err"), daemon=True).start()
    print("argv:", " ".join(argv))
    return p, q, raw


def drain(p, q, seconds, on_msg=None):
    seen, end = [], time.time() + seconds
    while time.time() < end:
        try:
            m = q.get(timeout=0.5)
        except queue.Empty:
            if p.poll() is not None and q.empty():
                break
            continue
        seen.append(m)
        if on_msg:
            on_msg(m)
    return seen


def texts(seen):
    out = []
    for m in seen:
        if m.get("type") == "assistant":
            for b in (m.get("message") or {}).get("content") or []:
                if isinstance(b, dict) and b.get("type") == "text":
                    out.append(b["text"])
    return " ".join(out)


def scenario_plaintext(tmp):
    """No --input-format at all. Two lines written with a gap: one prompt, or
    two turns?"""
    p, q, raw = spawn(tmp, [])
    p.stdin.write("Reply with exactly: FIRST\n"); p.stdin.flush()
    time.sleep(8)
    alive_mid = p.poll() is None
    print("still running 8s after the first line (i.e. waiting for more?):", alive_mid)
    try:
        p.stdin.write("Reply with exactly: SECOND\n"); p.stdin.flush()
        wrote_second = True
    except Exception as e:
        wrote_second = f"failed: {e}"
    print("second line written:", wrote_second)
    p.stdin.close()
    seen = drain(p, q, 90)
    results = [m for m in seen if m.get("type") == "result"]
    print("result messages:", len(results), "num_turns:", [r.get("num_turns") for r in results])
    print("assistant text:", texts(seen)[:300])
    print("=> plain stdin gives", "MULTIPLE turns" if len(results) > 1 else "ONE prompt")
    (HERE / "transcript-stdin-plaintext.jsonl").write_text(
        "".join(f"{t}\t{l}\n" for t, l in raw))


def scenario_control(tmp):
    """--permission-prompt-tool stdio WITHOUT --input-format stream-json.
    Does the control channel exist, and can an unframed stdin answer it?"""
    p, q, raw = spawn(tmp, ["--permission-prompt-tool", "stdio",
                            "Use the Write tool to create ctl.txt containing OK. Do not ask me anything first."])
    req = None
    end = time.time() + 120
    while time.time() < end and req is None:
        try:
            m = q.get(timeout=0.5)
        except queue.Empty:
            if p.poll() is not None and q.empty():
                break
            continue
        if m.get("type") == "control_request":
            req = m
            print("control_request ARRIVED without --input-format:", json.dumps(m)[:220])
        elif m.get("type") == "system" and m.get("subtype") == "permission_denied":
            print("permission_denied notice (the no-prompt-tool behaviour):", json.dumps(m)[:200])

    if req is None:
        print("=> NO control_request; the channel needs --input-format stream-json")
    else:
        resp = {"type": "control_response", "response": {
            "subtype": "success", "request_id": req["request_id"],
            "response": {"behavior": "allow",
                         "updatedInput": (req["request"] or {}).get("input", {})}}}
        try:
            p.stdin.write(json.dumps(resp) + "\n"); p.stdin.flush()
            print("answered on unframed stdin")
        except Exception as e:
            print("could not write the answer:", e)
    seen = drain(p, q, 60)
    for m in seen:
        if m.get("type") in ("result", "__stderr__", "__unparsed__"):
            print("  ", json.dumps(m, ensure_ascii=False)[:240])
    print("file created?", (pathlib.Path(tmp) / "ctl.txt").exists())
    try:
        p.stdin.close()
    except Exception:
        pass
    (HERE / "transcript-stdin-control.jsonl").write_text(
        "".join(f"{t}\t{l}\n" for t, l in raw))


def scenario_both(tmp):
    """Framed stdin carrying BOTH kinds: answer a control request, then send a
    further user turn down the same pipe."""
    p, q, raw = spawn(tmp, ["--input-format", "stream-json",
                            "--permission-prompt-tool", "stdio"])
    send = lambda o: (p.stdin.write(json.dumps(o) + "\n"), p.stdin.flush())
    send({"type": "user", "message": {"role": "user",
          "content": "Use the Write tool to create both.txt containing OK. Do not ask me anything first."}})
    req = None
    end = time.time() + 120
    while time.time() < end and req is None:
        try:
            m = q.get(timeout=0.5)
        except queue.Empty:
            if p.poll() is not None and q.empty():
                break
            continue
        if m.get("type") == "control_request":
            req = m
    print("control_request:", "yes" if req else "NO")
    if req:
        send({"type": "control_response", "response": {
            "subtype": "success", "request_id": req["request_id"],
            "response": {"behavior": "allow",
                         "updatedInput": (req["request"] or {}).get("input", {})}}})
    drain(p, q, 45)
    # ... and a second user turn on the same stdin the control response used.
    send({"type": "user", "message": {"role": "user", "content": "Reply with exactly: SECOND-TURN"}})
    seen = drain(p, q, 90)
    print("second turn text:", texts(seen)[:200])
    print("file created?", (pathlib.Path(tmp) / "both.txt").exists())
    try:
        p.stdin.close()
    except Exception:
        pass
    (HERE / "transcript-stdin-both.jsonl").write_text(
        "".join(f"{t}\t{l}\n" for t, l in raw))


if __name__ == "__main__":
    which = sys.argv[1]
    tmp = HERE / f"work-stdin-{which}"
    tmp.mkdir(exist_ok=True)
    {"plaintext": scenario_plaintext, "control": scenario_control,
     "both": scenario_both}[which](tmp)
