"use strict";

// The command line: tokenizer, verb dispatch (runVerbCommand) and the git /
// watcher helpers it calls, plus two shared text helpers (pad, agentLabel).

function pad(s, n) {
  s = String(s);
  return s.length >= n ? s : s + " ".repeat(n - s.length);
}

// agentLabel renders a TASK's agent identity the way `harness-cli ls` rows and
// the TUI table do: the resolved profile name, plus a "+skills" marker when the
// runner it was assigned to declares it injects the harness skill + inbox hook.
// An empty profile yields "" so each caller keeps its own placeholder.
//
// Mirrors cli.agentStr (Go) and tui.agentDescriptor (Go) — one grammar, one
// renderer per runtime. The marker reads off the TASK, never the runners array:
// a confined caller is served zero runners, so a runner-side lookup is blank for
// exactly the readers this marker exists for.
function agentLabel(profile, skillsInjected) {
  if (!profile) return "";
  return skillsInjected ? profile + "+skills" : profile;
}

// tokenize is a tiny quote-aware splitter. Single and double quotes group
// content as a single token; backslash escapes the next character. Unclosed
// quotes are treated as if closed at end-of-string (forgiving for dogfood).
function tokenize(line) {
  const out = [];
  let cur = "";
  let quote = "";
  let escaped = false;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (escaped) { cur += ch; escaped = false; continue; }
    if (ch === "\\") { escaped = true; continue; }
    if (quote) {
      if (ch === quote) { quote = ""; continue; }
      cur += ch;
      continue;
    }
    if (ch === '"' || ch === "'") { quote = ch; continue; }
    if (/\s/.test(ch)) {
      if (cur.length > 0) { out.push(cur); cur = ""; }
      continue;
    }
    cur += ch;
  }
  if (cur.length > 0) out.push(cur);
  return out;
}


// --- file ops dispatch -------------------------------------------------

// runGitAction hands a `git` sub-verb, already parsed by the shared
// declaration (cli/verb), to the Git tab.
//
// The argument loop that used to live here accepted EVERY git flag on EVERY
// sub-verb, because one loop served all of them -- so `git status <id> --rev X`
// parsed here and was refused by the CLI. It also read --max-bytes and threw
// it away ("caps are the runner's job"), which is worse than refusing it.
// Both are gone: the declaration knows which flags each sub-verb has.
async function runGitAction(taskID, g) {
  if (!window.__openGitTabFor) throw new Error("git: panel not ready");
  await window.__openGitTabFor(taskID, g.sub, {
    baseRev: g.baseRev, targetRev: g.targetRev, path: g.path,
    staged: g.staged, submodule: g.submodule, subrepo: g.subrepo,
    // `git file --rev X` resolves into targetRev; the panel reads `rev`.
    rev: g.sub === "file" ? g.targetRev : "",
    maxBytes: g.maxBytes, max: g.max,
  });
  return `git ${g.sub}: shown in the Git tab`;
}

// runFileAction executes a `file` sub-verb already parsed by the shared
// declaration (cli/verb) through the wasm bridge. Every argument loop that
// used to live in the fileXxxCmd helpers is gone; what remains here is the
// browser-side execution -- file pickers, progress bars, confirm dialogs --
// which is exactly the half that differs per surface.
// How runVerbCommand answers each declared path: the window.harness.* function
// it reaches, or a named cached value with the bound on its staleness.
// Design D13 + D17.
//
// The name has to be one the bridge actually exports -- it is a hand-written
// string, and four of these named functions that do not exist (filePush,
// filePull, fileEdit, fileNew, where the bridge has filePushBytes,
// filePullBytes and fileEditLoad). Nothing broke, because a descriptor is not
// called; it just stopped describing anything. The startup assertion and
// cmd_test.mjs both check the name resolves now.
//
// watcherListText is one verb's view of the shared watcher registry:
// `session await-idle ls` and `await-progress ls` each show their condition.
async function watcherListText(ctx, b, condition) {
  const all = await ctx.harness.awaitIdleList(b.flags.task || undefined);
  const ws = (Array.isArray(all) ? all : []).filter((w) => w.condition === condition);
  if (!ws.length) return "(no armed watchers)";
  if (b.flags.json) return ws.map((w) => JSON.stringify(w)).join("\n");
  return ws.map((w) => `#${w.watcherId}  ${String(w.taskId).slice(0, 8)}…  ${w.sink}${w.sink === "board" ? ` ${w.topic}` : ""}  by ${w.by}`).join("\n");
}

// RUNCMD_DISPATCH comes from the verb declarations, via the wasm bridge — it
// was a literal map here, and what kept it in step with the table was three
// assertions plus a page-local exception list duplicated between this file and
// cmd_test.mjs. Two of those assertions are structural now: a map built from
// PathsForSurface cannot name a verb the declaration does not give this
// surface, nor miss one it does.
//
// It is populated at startup rather than at load, because the bridge is what
// supplies it: main.js's IIFE assigns it once the wasm module is up, in the
// block that checks every entry names a function the bridge exports ("Every
// verb the declaration marks as reachable here must be dispatchable").
let RUNCMD_DISPATCH = {};

// HelpRequest is a -h on its way out of the switch below. It is not an error:
// the operator asked a question and the bridge answered it, so it unwinds past
// whichever case was parsing and is printed as that command's output.
class HelpRequest {
  constructor(lines) { this.text = lines.join("\n"); }
}

// parseOrHelp is the parse every case does, with the two non-Bound answers
// separated: a help request becomes output, a parse error stays an error.
//
// One helper rather than two lines per case because there are sixteen cases,
// and the shape it replaces (`if (b.error) throw`) is what made `submit -h`
// report the help text as a red failure -- the bridge had nowhere else to put
// it.
function parseOrHelp(ctx, tokens, opts) {
  const b = ctx.harness.parseCommand(tokens, opts || {});
  if (b.help) throw new HelpRequest(b.help);
  if (b.error) throw new Error(b.error);
  return b;
}

// runVerbCommand answers a -h from anywhere inside the dispatch below, and
// hands everything else through untouched.
async function runVerbCommand(tokens, ctx) {
  try {
    return await runVerbCommandDispatch(tokens, ctx);
  } catch (e) {
    if (e instanceof HelpRequest) return e.text;
    throw e;
  }
}

// runVerbCommandDispatch is the WebUI's command-line dispatch: one parsed verb
// in, the text to print out (or undefined when the case reported through
// ctx.echo itself). runVerbCommand above wraps it.
//
// Top level, like runFileAction and runGitAction above it, and for the reason
// those two were hoisted: nothing inside the page's IIFE can be executed
// outside a browser. It sat in there, and every WebUI drop an audit found was
// in the part no test could reach -- six flags on `submit` silently discarded,
// `exec ls` throwing on a field name the bridge does not emit, five `session
// stream` paths referencing an undeclared variable. webui/static/cmd_test.mjs
// now drives this against the REAL wasm bridge under `node --test`.
//
// ctx is everything the page owns and a test does not: the compose dropdowns,
// the snapshot cache, the panels. Every one is a function so the test can
// record the call rather than mimic the widget.
async function runVerbCommandDispatch(tokens, ctx) {
  const cmd = tokens[0];
  let out;
  switch (cmd) {
    case "submit": {
      // Parsed by the shared declaration. The repo comes from the Compose
      // dropdown through the surface-context tier of --repo's ladder, the
      // same ladder the CLI reads env and workspace config from -- one
      // ladder with three injections, instead of three ladders.
      const dropdownRepo = ctx.composeRepo();
      const dropdownHost = ctx.composeHost();
      const b = parseOrHelp(ctx, tokens, {
        repo: dropdownRepo, host: dropdownHost,
        agent: ctx.composeAgent(),
      });
      // b.flags.repo is already the ladder's answer -- typed flag first,
      // dropdown second -- because parseCommand applies the
      // surface-context tier from the object above. Both were read
      // straight off the dropdown before, so `submit --repo /not/a/repo`
      // queued against the dropdown's repo instead.
      const repo = b.flags.repo || dropdownRepo;
      const resumeTaskId = b.flags.resume || ctx.resumeTaskID();
      if (!repo && !resumeTaskId) {
        throw new Error("no runner selected (pick one from the dropdown, or fill in Resume task id)");
      }
      const task = b.flags.task || b.trail;
      // --exact runs the task's recorded prompt; the declaration already
      // refuses one typed beside it.
      if (!task && !b.flags.exact) throw new Error("submit: missing task prompt");
      const scopeFor = (b.custom && b.custom["scope-for"]) || null;
      const req = ctx.sessionReq({
        repo, task,
        host: b.flags.host || dropdownHost,
        // The three runner selectors are mutually exclusive in the
        // declaration, so at most one of these is non-empty.
        runner: b.flags.runner || "",
        ip: b.flags.ip || "",
        agent: b.flags.agent || ctx.composeAgent(),
        agentArgs: (b.custom && b.custom["agent-arg"]) || ctx.agentArgs(),
        // b.set, not truthiness: --caps none is a real grant of nothing.
        caps: b.set && b.set.caps ? b.flags.caps : null,
        scope: b.set && b.set.scope ? b.flags.scope : null,
        scopeFor: scopeFor && scopeFor.length ? scopeFor : null,
        delegateCaps: b.set && b.set["delegate-caps"] ? b.flags["delegate-caps"] : null,
        delegateScope: b.set && b.set["delegate-scope"] ? b.flags["delegate-scope"] : null,
        resumeTaskId,
        resumeConversation: !!b.flags["resume-conversation"],
        resumeExact: !!b.flags.exact,
      });
      try {
        out = await ctx.harness.submit(req);
      } catch (e) {
        // More than one (runner, agent) choice matched: the same picker the
        // interactive opens use, re-issuing THIS submit pinned.
        if (ctx.routeAmbiguous(e, req, async (r) => {
          ctx.echo(await ctx.harness.submit(r));
        })) {
          out = "submit: more than one runner/agent matches — pick a runner";
          break;
        }
        throw e;
      }
      break;
    }
    case "conns": {
      // The tab and, with --trsf, the reading panel inside it. Nothing is
      // fetched here: the tab is already fed by the snapshot poll, and the
      // panel starts its own read when it expands.
      const b = parseOrHelp(ctx, tokens, {});
      ctx.connsView({
        trsf: !!b.flags.trsf,
        runner: b.flags.runner || "",
        // --client was parsed and dropped here, so `conns --trsf --client`
        // aimed the panel at the server. The declaration makes the two
        // mutually exclusive, so at most one of these is set.
        client: b.flags.client || "",
        watch: b.flags.watch || "",
      });
      out = b.flags.trsf ? "conns: trsf reading open" : "conns: showing the connections tab";
      break;
    }
    case "scope":
    case "caps": {
      // Which of the three, by the SUB-VERB word rather than by counting
      // tokens: `caps --json` has two tokens and is still the catalog, and a
      // count test dropped the declared flag while reporting "only ... are
      // available here".
      const sub = cmd === "caps" ? (tokens[1] || "") : "";
      // `caps set-defaults`, and `scope` -- its shorter declared spelling --
      // set what a spawn from this page carries when the compose panel's own
      // controls are not overridden. Same state as the chips (D17: one idea,
      // one answer), so this drives them rather than keeping a second copy.
      if (cmd === "scope" || sub === "set-defaults") {
        const b = parseOrHelp(ctx, tokens, {});
        const raw = {
          caps: b.flags.caps || "",
          scope: b.flags.scope || "",
          scopeFor: b.flags["scope-for"] || [],
          delegateCaps: b.flags["delegate-caps"] || "",
          delegateScope: b.flags["delegate-scope"] || "",
        };
        if (!raw.caps && !raw.scope && !raw.delegateCaps && !raw.delegateScope) {
          // Naming nothing is a question. The TUI opens its picker; here the
          // panel is already on screen, so the answer is the current value.
          const d = ctx.spawnDefaults();
          out = "caps: " + d.capsLabel + "\nscope: " + (d.scope || "subtree (default)") +
            "\ndelegate: " + d.delegateCapsLabel + "\ndelegate-scope: " + d.delegateScope;
          break;
        }
        const parsed = ctx.harness.parseAuthority(raw);
        if (parsed.error) throw new Error(parsed.error);
        ctx.setSpawnDefaults(parsed);
        const lines = [];
        if (parsed.capsLabel !== undefined) lines.push("caps set: " + parsed.capsLabel);
        if (parsed.scopeLabel !== undefined) lines.push("scope set: " + parsed.scopeLabel);
        if (parsed.delegateCapsLabel !== undefined) lines.push("delegate set: " + parsed.delegateCapsLabel);
        if (parsed.delegateScopeLabel !== undefined) lines.push("delegate-scope set: " + parsed.delegateScopeLabel);
        out = lines.join("\n");
        break;
      }
      if (sub === "reach") {
        // What one task reaches: the CLI's report, from the same writer. The
        // tree's reach mode draws the same answer.
        const b = parseOrHelp(ctx, tokens, {});
        out = await ctx.harness.capsReachText({
          taskId: b.args[0], cap: b.flags.cap || "", json: !!b.flags.json, tree: !!b.flags.tree,
        });
        break;
      }
      if (sub === "what-if") {
        // What a caps set, a spawn or a set-parent would change: the CLI's
        // report over a fresh listing. Only the flags given cross, under
        // their declared names, and the bridge parses them through the same
        // declaration again. Nothing is sent.
        const b = parseOrHelp(ctx, tokens, {});
        const req = { taskId: b.args[0] };
        for (const k of Object.keys(b.set || {})) {
          req[k] = (b.custom && b.custom[k]) || b.flags[k];
        }
        out = await ctx.harness.capsWhatIfText(req);
        break;
      }
      if (sub === "audit") {
        // Who holds which capability: the CLI's text, from the same writer.
        // The 権限 tab draws the same rows.
        const b = parseOrHelp(ctx, tokens, {});
        out = await ctx.harness.capsAuditText({
          all: !!b.flags.all, empty: !!b.flags.empty,
          cap: b.flags.cap || "", json: !!b.flags.json,
        });
        break;
      }
      if (sub !== "set-parent") {
        // Everything else on `caps` is the catalog: every grantable
        // capability with the sentence saying what it gates, from the same
        // verb.WriteCaps the CLI prints. The chips carry the names and none
        // of the sentences. A word that is not a declared sub-verb reaches
        // the declaration and is refused there, rather than by a second list
        // of sub-verbs kept here.
        const b = parseOrHelp(ctx, ["caps", ...tokens.slice(1)], {});
        const text = ctx.harness.capsCatalog(!!b.flags.json);
        if (text && text.error) throw new Error(text.error);
        out = text;
        break;
      }

      // `caps set-parent` on the other surfaces; the shared declaration
      // enforces "exactly one of --parent / --none / --swap".
      const b = parseOrHelp(ctx, ["caps", "set-parent", ...tokens.slice(2)], {});
      const req = { taskId: b.args[0] };
      if (b.flags.swap) req.swap = true;
      else if (b.flags.none) req.parentId = "";
      else req.parentId = b.flags.parent;
      out = ctx.setParentMessage(
        b.args[0], req.swap === true, await ctx.harness.setParent(req));
      break;
    }
    case "ls": {
      // The shared listing, not the filtered pane. This case used to be
      // spelled `list` and scraped .task-row out of the DOM -- the
      // FILTERED render -- while `harness-cli ls` returned the whole
      // snapshot: one idea, two answers, which is what D17 forbids.
      // cli.Client.List was already exposed here as harness.list and
      // simply went unused.
      const b = parseOrHelp(ctx, tokens, {});
      if (b.flags.filtered) {
        await ctx.refreshSnapshot();
        out = ctx.filteredTaskRows();
      } else {
        // --json and --tree reach cli.Client's own renderers; both parsed
        // here and were dropped while only the row form was exposed.
        out = await ctx.harness.list(
          b.flags.json ? "json" : b.flags.tree ? "tree" : "");
      }
      break;
    }
    case "refresh":
    case "sync":
      // Force a snapshot re-sync without echoing the rows (TUI parity).
      await ctx.refreshSnapshot();
      out = "snapshot refreshed";
      break;
    case "cancel": {
      const b = parseOrHelp(ctx, tokens, {});
      await ctx.harness.cancel(b.args[0]);
      out = "cancelled";
      break;
    }
    case "preview":
      if (!tokens[1]) throw new Error("preview: missing task id");
      ctx.openSessionPreview(tokens[1]);
      out = `preview ${tokens[1].slice(0, 12)}…`;
      break;
    case "grid": {
      // The grid grammar was already shared (cli.ParseGridArgs, which the
      // workspace config validates against too); this routes the command
      // input through the same declaration as everything else.
      const b = parseOrHelp(ctx, tokens, {});
      // The declaration already refused --descendants without --under, so
      // the mode follows from what survived the parse.
      const under = b.flags.under || "";
      // Full ids only. The TUI resolves a prefix against its task list
      // (tui/app.go's GridAction case) and this surface has no resolver,
      // so a half-typed id would silently select nothing.
      const full = (id) => /^[0-9a-fA-F]{32}$/.test(id);
      if (under && !full(under)) throw new Error("grid: --under needs a full 32-hex task id");
      const bad = b.args.filter((id) => !full(id));
      if (bad.length) throw new Error(`grid: not a full 32-hex task id: ${bad.join(", ")}`);
      const mode = under ? (b.flags.descendants ? "descendants" : "subtree")
                 : (b.args.length ? "ids" : "all");
      out = await ctx.openGridSet({ mode, anchor: under, ids: b.args });
      break;
    }
    case "restore": {
      // The undo half of prune. Ids only -- there is no sweep back, because
      // the WAL holds every task the server has ever seen.
      const b = parseOrHelp(ctx, tokens, {});
      // The bare form lists: the ids live in a file on the server host that
      // no other surface reads, so a restore that only ACTED would be usable
      // by someone who wrote the id down before the accident.
      out = await ctx.harness.restore(b.flags.list ? [] : b.args);
      break;
    }
    case "prune": {
      // Parsed by the shared declaration (cli/verb) through the wasm
      // bridge, so this surface cannot drift from the grammar the CLI and
      // the TUI parse. The hand-written --before / -f loop that used to
      // live here was a third independent copy of it, and the one that
      // spelled the task listing `list` while the others said `ls`.
      const b = parseOrHelp(ctx, tokens, {});
      if (b.args.length > 0) {
        out = await ctx.harness.prune({ taskIds: b.args, force: !!b.flags.force });
      } else {
        out = await ctx.harness.prune({ before: b.flags.before });
      }
      break;
    }
    case "file": {
      // Parsed by the shared declaration through the wasm bridge. The
      // per-sub-verb argument loops this case used to reach were a third
      // copy of this grammar -- and the copy where `file push` quietly
      // accepted no flags at all while the CLI and TUI took -r/-f/-p.
      const b = parseOrHelp(ctx, tokens, {});
      out = await runFileAction(b);
      break;
    }
    case "git": {
      // `git <sub> <task-id> ...`, the same shape as `file ls <task-id>`.
      // The id is an ordinary positional now, so the bridge parses the line
      // like any other and this page holds no knowledge of its position.
      const g = ctx.harness.parseGit(tokens);
      if (g.help) throw new HelpRequest(g.help);
      if (g.error) throw new Error(g.error);
      out = await runGitAction(g.taskId, g);
      break;
    }
    case "runner": {
      // runner enroll / keys / unenroll / kick and runner dial add / ls / rm:
      // the text is rendered by the same Go function the CLI prints with, so
      // the three surfaces say one thing.
      const b = parseOrHelp(ctx, tokens, {});
      let r;
      switch (b.path[1]) {
        case "enroll": r = await ctx.harness.runnerEnroll(b.args[0], b.flags.label || ""); break;
        case "keys": r = await ctx.harness.runnerKeys(); break;
        case "unenroll": r = await ctx.harness.runnerUnenroll(b.args[0], !!b.flags.force); break;
        case "kick": r = await ctx.harness.runnerKick(b.args[0], !!b.flags.force); break;
        case "dial":
          switch (b.path[2]) {
            case "add": r = await ctx.harness.runnerDialAdd(b.args[0], b.flags.key || "", b.flags.label || ""); break;
            case "ls": r = await ctx.harness.runnerDialList(); break;
            case "rm": r = await ctx.harness.runnerDialRemove(b.args[0]); break;
            default: throw new Error(`runner dial: unknown sub-verb ${b.path[2]}`);
          }
          break;
        default: throw new Error(`runner: unknown sub-verb ${b.path[1]}`);
      }
      if (!r.ok) throw new Error(r.text.trimEnd());
      out = r.text.trimEnd();
      break;
    }
    case "server": {
      const b = parseOrHelp(ctx, tokens, {});
      const status = await ctx.harness.serverDialRunner(b.args[0], b.flags.via || undefined);
      out = `server dial-runner ${b.args[0]}${b.flags.via ? ` --via=${b.flags.via}` : ""}: ${status}`;
      break;
    }
    // Same words the TUI cmdline takes, so a hand that learned one surface
    // knows the other. The sub-verb is decided by the FIRST token only:
    // `exec <id> ls -la` is a command named ls, because a task id
    // introduced it.
    // The progress twin of `session await-idle`: top-level because a oneshot
    // task, which has no session, writes progress too. ls/kill share the
    // watcher registry with await-idle, so they call the same bridge.
    case "await-progress": {
      const b = parseOrHelp(ctx, tokens, {});
      const sub = b.path.length > 1 ? b.path[1] : "arm";
      if (sub === "ls") {
        out = await watcherListText(ctx, b, "progress");
        break;
      }
      if (sub === "kill") {
        const failed = [];
        for (const id of b.args) {
          try { await ctx.harness.awaitIdleKill(Number(id)); }
          catch (e) { failed.push(`${id}: ${e.message}`); }
        }
        if (failed.length) throw new Error(`await-progress kill: ${failed.join("; ")}`);
        out = `killed watcher ${b.args.join(", ")}`;
        break;
      }
      const sink = b.flags.notify ? "notify" : (b.flags.topic ? "board" : "reply");
      if (sink === "reply") ctx.echo("await-progress: waiting for the task's next progress line…");
      const r = await ctx.harness.awaitProgress({
        taskId: b.args[0], since: b.flags.since || 0, sink, topic: b.flags.topic || "",
      });
      const st = r && r.status ? r.status : "";
      out = `await-progress ${String(b.args[0]).slice(0, 12)}: ${st}` +
        (r && r.status === "fired" ? ` ${JSON.stringify(r.progress)}` : "") +
        (r && r.watcherId ? ` (watcher ${r.watcherId})` : "");
      break;
    }
    case "exec": {
      // Parsed by the shared declaration: --shell/--sshd-parent, the
      // optional `--` before the argv, and the refusal of `exec --shell
      // kill 3` (a sub-verb where a task id belongs) all come from there.
      const b = parseOrHelp(ctx, tokens, {});
      const sub = b.path.length > 1 ? b.path[1] : "run";
      if (sub === "ls") {
        const es = await ctx.harness.execRunList(b.flags.task || undefined);
        // execId / taskId / argvText are what harnessExecRunList emits
        // (cmd/harness-webui-wasm/main.go). Reading exec_id / task / argv
        // gave undefined, and `.slice(0,8)` on it threw as soon as one
        // exec was running -- so this path only ever worked when empty.
        out = es.length
          ? (b.flags.json
              ? es.map((e) => JSON.stringify(e)).join("\n")
              : es.map((e) => `#${e.execId}  ${String(e.taskId).slice(0, 8)}…  ${e.argvText}\n    ${e.traffic || ""}`).join("\n"))
          : "(no running execs)";
      } else if (sub === "kill") {
        // Every id, as on the CLI: this killed args[0] and reported
        // "killed exec 1" for `exec kill 1 2 3`.
        // Every id, even after one fails: an `await` in the loop aborted the
            // rest, and the TUI killed them.
            const failedexecRunKill = [];
            for (const id of b.args) {
              try { await ctx.harness.execRunKill(Number(id)); }
              catch (e) { failedexecRunKill.push(`${id}: ${e.message}`); }
            }
            if (failedexecRunKill.length) throw new Error(`exec kill: ${failedexecRunKill.join("; ")}`);
        out = `killed exec ${b.args.join(", ")}`;
      } else if (sub === "tap") {
        // Same panel the row's button opens: one rendering path for one
        // subscription.
        const id = Number(b.args[0]);
        const e = ctx.execs().find((x) => x.exec_id === id);
        if (!e) throw new Error(`exec tap: no such exec ${id} in the current snapshot`);
        const entry = ctx.findExecEntry(id);
        if (!entry) throw new Error(`exec tap: exec ${id} has no row to attach a panel to`);
        ctx.toggleExecTap(e, entry.wrap, entry.button, {
          chan: b.flags.chan || "all",
          maxBytes: b.flags["max-bytes"] || 0,
        });
        out = ctx.execTapOpen(id) ? `tapping exec ${id}` : `stopped tapping exec ${id}`;
      } else {
        out = await ctx.execRunToOutput(b.args[0], b.trailArgs, { shell: !!b.flags.shell, sshdParent: !!b.flags["sshd-parent"] });
      }
      break;
    }
    case "forward": {
      // forward ls renders from the snapshot the page already polls -- no
      // extra RPC, except for --drops, which asks something the snapshot does
      // not (see forwardLsView). Starting a socket-bound forward is CLI-only
      // (a browser cannot bind a local listener), which the declaration says
      // by leaving the open form off this surface.
      const b = parseOrHelp(ctx, tokens, {});
      const sub = b.path[1];
      if (sub === "ls") {
        const fwds = await ctx.forwardLsView({
          task: b.flags.task || "",
          // --drops was parsed and dropped here: it would have read the
          // snapshot, which never asked the endpoints, so every hop key was
          // absent no matter what was typed.
          drops: !!b.flags.drops,
        });
        out = fwds.length
          ? (b.flags.json
              ? fwds.map((f) => JSON.stringify(f)).join("\n")
              : fwds.map((f) => `#${f.forward_id}  ${f.dir}  ${f.task.slice(0, 8)}…  ${f.spec}  ${f.origin}\n    ${f.traffic || ""}`).join("\n"))
          : "(no active port forwards)";
      } else if (sub === "kill") {
        // Every id, even after one fails: an `await` in the loop aborted the
            // rest, and the TUI killed them.
            const failedforwardKill = [];
            for (const id of b.args) {
              try { await ctx.harness.forwardKill(Number(id)); }
              catch (e) { failedforwardKill.push(`${id}: ${e.message}`); }
            }
            if (failedforwardKill.length) throw new Error(`forward kill: ${failedforwardKill.join("; ")}`);
        out = `killed forward ${b.args.join(", ")}`;
      } else {
        // The command input starts the same tap the row's button does, and
        // shows it in the same panel: a second rendering path for the same
        // subscription is how two surfaces end up disagreeing.
        const id = Number(b.args[0]);
        const f = ctx.forwards().find((x) => x.forward_id === id);
        if (!f) throw new Error(`forward tap: no such forward ${id} in the current snapshot`);
        const entry = ctx.findForwardEntry(id);
        if (!entry) throw new Error(`forward tap: forward ${id} has no row to attach a panel to`);
        ctx.toggleForwardTap(f, entry.wrap, entry.button, {
          dir: b.flags.dir || "both",
          maxBytes: b.flags["max-bytes"] || 0,
        });
        out = ctx.tapOpen(id) ? `tapping forward ${id}` : `stopped tapping forward ${id}`;
      }
      break;
    }
    // The `session` family reaches this surface for the first time here,
    // and only its `stream` namespace: the lifecycle verbs (new/ls/kill)
    // have buttons, while these four had no route at all. Recorded as a
    // partial family rather than pretending the rest exists.
    case "session": {
      // `session await-idle` is the canonical path on every surface; the
      // WebUI's old top-level `await-idle` spelling is gone (D16).
      if (tokens[1] === "await-idle") {
        const b = parseOrHelp(ctx, ["session", "await-idle", ...tokens.slice(2)], {});
        const sub = b.path.length > 2 ? b.path[2] : "arm";
        if (sub === "ls") {
          out = await watcherListText(ctx, b, "idle");
          break;
        }
        if (sub === "kill") {
          // Every id, even after one fails, as `exec kill`.
          const failed = [];
          for (const id of b.args) {
            try { await ctx.harness.awaitIdleKill(Number(id)); }
            catch (e) { failed.push(`${id}: ${e.message}`); }
          }
          if (failed.length) throw new Error(`await-idle kill: ${failed.join("; ")}`);
          out = `killed await-idle watcher ${b.args.join(", ")}`;
          break;
        }
        // Parsed by the shared declaration, which also refuses --notify
        // with --topic: two sinks for one fire.
        const sink = b.flags.notify ? "notify" : (b.flags.topic ? "board" : "reply");
        if (sink === "reply") ctx.echo("await-idle: waiting for the session to go idle…");
        const r = await ctx.harness.awaitIdle({
          taskId: b.args[0], thresholdMs: b.flags["threshold-ms"] || 0,
          sink, topic: b.flags.topic || undefined,
        });
        out = `await-idle ${b.args[0].slice(0, 12)}: ${r.status}${r.watcherId ? ` (watcher ${r.watcherId})` : ""}`;
        break;
      }
      if (tokens[1] !== "stream") {
        ctx.echo("session: only the `stream` namespace is available here — new/ls/kill are the buttons above");
        break;
      }
      // Parsed by the shared declaration like every other verb here. It
      // was a hand-written token walk, whose stated reason -- "this input
      // is whitespace-split with no flag parser" -- expired when
      // parseCommand arrived; what it left behind was a `--allow` the CLI
      // accepts and this surface swallowed as text, and a reference to an
      // undeclared `args` that made all five paths throw ReferenceError.
      const verb = tokens[2] || "";
      if (verb === "requests" || verb === "snapshot") {
        ctx.echo(`session stream ${verb}: specified (design §3) but not built yet`);
        break;
      }
      // The task id defaults to the open chat, so it is spliced in before
      // the parse rather than after: the declaration has no notion of
      // "whatever is on screen".
      const rest = tokens.slice(3);
      // Only when the line starts with the id, or with nothing. These are
      // Trailing verbs -- everything after the positional is literal text --
      // so a line whose first token is a FLAG has the id somewhere this
      // surface cannot find without re-implementing the flag parse, and
      // guessing put it in the wrong place: `session stream turn --flush-ms
      // 900 <id> hi` spliced the OPEN CHAT's id and typed
      // "--flush-ms 900 <id> hi" into that agent. Wrong task, wrong text.
      let sid;
      if (rest[0] && /^[0-9a-fA-F]{32}$/.test(rest[0])) {
        sid = rest.shift();
      } else if (rest[0] && rest[0].startsWith("-")) {
        ctx.echo(`session stream ${verb}: name the task id after the flags ` +
                 `(the id only defaults to the open chat when it leads the line)`);
        break;
      } else {
        sid = ctx.chatTaskID();
      }
      if (!sid) { ctx.echo("session stream: a task id is required (or open a chat first)"); break; }
      const b = parseOrHelp(ctx, ["session", "stream", verb, sid, ...rest], {});
      try {
        switch (verb) {
          case "turn":
            await ctx.harness.streamTurn(sid, b.trail);
            ctx.echo(`stream turn ${sid.slice(0, 8)}: sent`);
            break;
          case "approve": {
            const verdict = b.flags.allow ? "allow" : "deny";
            await ctx.harness.streamApprove(
              sid, b.args[1], verdict, b.flags.message || "",
              b.set.suggestion ? b.flags.suggestion : -1);
            ctx.echo(`stream approve ${sid.slice(0, 8)} ${b.args[1]}: ${verdict}`);
            break;
          }
          case "answer": {
            const specs = (b.custom && b.custom["answer"]) || [];
            await ctx.harness.streamAnswer(sid, b.args[1], specs, b.flags.reply || "");
            ctx.echo(`stream answer ${sid.slice(0, 8)} ${b.args[1]}: sent`);
            break;
          }
          case "interrupt":
            await ctx.harness.streamInterrupt(sid);
            ctx.echo(`stream interrupt ${sid.slice(0, 8)}: sent`);
            break;
          case "finish":
            await ctx.harness.streamFinish(sid);
            ctx.echo(`stream finish ${sid.slice(0, 8)}: sent`);
            break;
          case "attach":
            await ctx.openChatFor(sid);
            ctx.echo(`stream attach ${sid.slice(0, 8)}: chat opened`);
            break;
          default:
            ctx.echo(`unknown session stream verb ${JSON.stringify(verb)}`);
        }
      } catch (err) {
        ctx.echo(`session stream ${verb}: ${err.message}`);
      }
      break;
    }

    case "help":
      // Generated from the verb declarations, via the wasm bridge. This was
      // ~56 literal strings here plus a shortened copy in index.html's
      // placeholder, and nothing pinned either: `--via <cid>` survived here
      // after the flag started taking a runner identity, while the CLI and TUI
      // stayed right because their usage comes from the table. The per-verb
      // sentences moved into the table as SurfaceNotes[WebUI], so they sit
      // beside the verb they describe.
      out = ["commands:"].concat(ctx.harness.help()).join("\n");
      break;
    default:
      out = `unknown command: ${cmd} (type 'help' for the list)`;
  }
  return out;
}
