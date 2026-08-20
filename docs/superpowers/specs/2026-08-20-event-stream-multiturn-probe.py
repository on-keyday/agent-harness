"""Three turns down one framed stdin: does each get a result, does the
session_id hold, and does context carry across turns?"""
import json, os, subprocess, sys, threading, time, queue
env = dict(os.environ)
for k in list(env):
    if k.startswith(("CLAUDE_CODE_","HARNESS_")) or k in ("CLAUDECODE","AI_AGENT"): env.pop(k)
USE_P = "--no-p" not in sys.argv
argv = ["claude"] + (["-p"] if USE_P else []) + ["--input-format","stream-json","--output-format","stream-json",
                      "--verbose","--model","claude-haiku-4-5-20251001"]
print("argv:", " ".join(argv))
p = subprocess.Popen(argv, cwd="/tmp", env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=subprocess.PIPE, text=True, bufsize=1)
q=queue.Queue()
threading.Thread(target=lambda:[q.put(l) for l in p.stdout],daemon=True).start()
def send(t): p.stdin.write(json.dumps({"type":"user","message":{"role":"user","content":t}})+"\n"); p.stdin.flush()

turns = ["Remember the codeword TANGERINE. Reply with just: OK",
         "Reply with just the number 42.",
         "What codeword did I give you? Reply with just the codeword."]
sessions, results, texts = set(), [], []
for i,t in enumerate(turns,1):
    send(t)
    got_result=False; end=time.time()+120; buf=[]
    while time.time()<end and not got_result:
        try: line=q.get(timeout=1)
        except queue.Empty:
            if p.poll() is not None: break
            continue
        try: m=json.loads(line)
        except Exception: continue
        if m.get("session_id"): sessions.add(m["session_id"])
        if m.get("type")=="assistant":
            for b in (m.get("message") or {}).get("content") or []:
                if isinstance(b,dict) and b.get("type")=="text": buf.append(b["text"])
        if m.get("type")=="result":
            got_result=True
            results.append({"turn":i,"subtype":m.get("subtype"),"num_turns":m.get("num_turns")})
    texts.append(" ".join(buf).strip()[:60])
    print(f"turn {i}: result={'yes' if got_result else 'NO'}  text={texts[-1]!r}")

print("\ndistinct session_ids:", len(sessions), list(sessions)[:2])
print("results:", results)
print("context carried across turns:", "TANGERINE" in texts[-1].upper())
p.stdin.close(); p.wait(timeout=60); print("exit:", p.returncode)
