#!/usr/bin/env python3
"""Does --continue / --resume work under --input-format stream-json?

The harness already resumes conversations with --continue on the oneshot path
(scripts/agent_presets.py resumeOneshotArgv). Whether that composes with a
framed stdin is untested, and it decides whether the event-stream kind needs
--session-id at all.

Run: probe_resume.py           (does the whole sequence, one nonce)
"""
import json, os, subprocess, threading, time, queue, pathlib, uuid, sys

HERE = pathlib.Path(__file__).parent
MODEL = "claude-haiku-4-5-20251001"
WORK = HERE / "work-resume"


def run(workdir, extra, turns, wait=120, label=""):
    """Spawn with a framed stdin, send each turn, return (texts, init_ids)."""
    argv = ["claude", "-p", "--output-format", "stream-json", "--verbose",
            "--model", MODEL, "--input-format", "stream-json", *extra]
    env = dict(os.environ)
    for k in list(env):
        if k.startswith(("CLAUDE_CODE_", "HARNESS_")) or k in ("CLAUDECODE", "AI_AGENT"):
            env.pop(k, None)
    p = subprocess.Popen(argv, cwd=str(workdir), env=env,
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                         stderr=subprocess.PIPE, text=True, bufsize=1)
    q, raw = queue.Queue(), []

    def pump(s, tag):
        for line in s:
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

    print(f"\n--- {label}\n    argv: {' '.join(argv[1:])}")
    texts, inits, errs, results = [], [], [], 0
    for t in turns:
        p.stdin.write(json.dumps(
            {"type": "user", "message": {"role": "user", "content": t}}) + "\n")
        p.stdin.flush()
        end = time.time() + wait
        while time.time() < end:
            try:
                m = q.get(timeout=0.5)
            except queue.Empty:
                if p.poll() is not None and q.empty():
                    break
                continue
            ty = m.get("type")
            if ty == "system" and m.get("subtype") == "init":
                inits.append(m.get("session_id"))
            elif ty == "assistant":
                for b in (m.get("message") or {}).get("content") or []:
                    if isinstance(b, dict) and b.get("type") == "text":
                        texts.append(b["text"])
            elif ty == "__stderr__":
                errs.append(m["line"])
            elif ty == "result":
                results += 1
                break
    try:
        p.stdin.close()
        p.wait(timeout=20)
    except Exception:
        p.kill()
    (HERE / f"transcript-resume-{label.split()[0]}.jsonl").write_text(
        "".join(f"{t}\t{l}\n" for t, l in raw))
    print("    exit:", p.returncode, "| results:", results, "| session_ids:", inits)
    for e in errs[:4]:
        print("    stderr:", e[:160])
    return " ".join(texts), inits


def main():
    WORK.mkdir(exist_ok=True)
    nonce = "ZEBRA-" + uuid.uuid4().hex[:6].upper()
    chosen = str(uuid.uuid4())

    # 1. Establish a conversation, with a session id WE picked.
    t1, i1 = run(WORK, ["--session-id", chosen],
                 [f"Remember this codeword and reply with just the word OK: {nonce}"],
                 label="seed (--session-id chosen)")
    print("    chose:", chosen, "| honoured:", chosen in i1)
    print("    said:", t1[:120])

    # 2. --continue, framed stdin. Does it recall, and which session?
    t2, i2 = run(WORK, ["--continue"],
                 ["What codeword did I ask you to remember? Reply with just the codeword."],
                 label="continue (--continue + stream-json in)")
    print("    recalled:", nonce in t2, "|", t2[:120])

    # 3. --resume <id>, framed stdin.
    t3, i3 = run(WORK, ["--resume", chosen],
                 ["What codeword did I ask you to remember? Reply with just the codeword."],
                 label="resume (--resume <uuid> + stream-json in)")
    print("    recalled:", nonce in t3, "|", t3[:120])

    # 4. Reusing an id that already exists -- what the harness would do if it
    #    keyed a session id off a task id and resumed twice.
    t4, i4 = run(WORK, ["--session-id", chosen],
                 ["Reply with just: SECOND-USE"],
                 label="reuse (--session-id of an existing session)")
    print("    said:", t4[:160])

    print("\n=== summary")
    print("  --session-id honoured :", chosen in i1)
    print("  --continue recalls    :", nonce in t2, "(session ids seen:", i2, ")")
    print("  --resume recalls      :", nonce in t3, "(session ids seen:", i3, ")")
    print("  --session-id reused   :", "ok" if t4.strip() else "refused/empty", i4)


if __name__ == "__main__":
    main()
