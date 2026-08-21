"""Can more than one approval be pending at once?

Ask for several tool calls in one turn and answer NOTHING. Count how many
can_use_tool requests arrive before any response is sent. If the CLI blocks on
one at a time, exactly one arrives and the rest wait behind it.
"""
import json, os, subprocess, threading, time, queue, pathlib, sys
work = pathlib.Path("/tmp/probe-concurrent"); work.mkdir(exist_ok=True)
env = dict(os.environ)
for k in list(env):
    if k.startswith(("CLAUDE_CODE_","HARNESS_")) or k in ("CLAUDECODE","AI_AGENT"): env.pop(k)
p = subprocess.Popen(["claude","-p","--input-format","stream-json","--output-format","stream-json",
                      "--verbose","--model","claude-haiku-4-5-20251001",
                      "--permission-prompt-tool","stdio"],
                     cwd=str(work), env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=subprocess.PIPE, text=True, bufsize=1)
q=queue.Queue()
threading.Thread(target=lambda:[q.put(l) for l in p.stdout],daemon=True).start()
p.stdin.write(json.dumps({"type":"user","message":{"role":"user","content":
  "Create three files in this directory using the Write tool, in ONE batch of parallel tool calls: "
  "a.txt containing A, b.txt containing B, c.txt containing C. Do not ask me anything."}})+"\n")
p.stdin.flush()

reqs=[]; tool_uses=0; end=time.time()+120
while time.time()<end:
    try: line=q.get(timeout=1)
    except queue.Empty:
        if p.poll() is not None: break
        continue
    try: m=json.loads(line)
    except Exception: continue
    if m.get("type")=="control_request" and (m.get("request") or {}).get("subtype")=="can_use_tool":
        r=m["request"]
        reqs.append((m["request_id"], r.get("tool_name"), json.dumps(r.get("input"))[:60]))
        print(f"  request #{len(reqs)}: {r.get('tool_name')} {reqs[-1][2]}")
    if m.get("type")=="assistant":
        for b in (m.get("message") or {}).get("content") or []:
            if isinstance(b,dict) and b.get("type")=="tool_use": tool_uses+=1
    # answer NOTHING; wait to see whether more pile up
    if len(reqs)>=1 and time.time()>end-95 and len(reqs)>=3: break

print(f"\ntool_use blocks in the turn: {tool_uses}")
print(f"can_use_tool requests OUTSTANDING with zero answers sent: {len(reqs)}")
print("=> concurrent pending possible:", len(reqs) > 1)
p.kill()
