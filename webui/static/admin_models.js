"use strict";

// Models of the admin screens: actions (define, edit, run), the action
// checklist of a grant condition, and runner keys. Plain functions over plain
// objects, so `node --test` reaches them (admin_test.mjs). Every rule the
// server enforces is asked of Go through the bridge (harness.actionFormCheck,
// harness.actionTokenValid) rather than restated here: a copy of a rule has no
// way to fail loudly when the rule changes.

// actionFormModel is the editable state of the Actions form: a copy of view
// (an edit at view's revision), or an empty new action whose command is one
// literal word.
function actionFormModel(view) {
  if (!view) {
    return { ref: "", revision: 0, name: "", description: "", params: [], template: [{ literal: "", param: "", isParam: false }] };
  }
  return {
    ref: view.id,
    revision: view.revision,
    name: view.name,
    description: view.description || "",
    params: (view.params || []).map((p) => ({ name: p.name, type: p.type, choices: [...(p.choices || [])] })),
    template: (view.template || []).map((e) => ({ literal: e.literal || "", param: e.param || "", isParam: !!e.isParam })),
  };
}

// actionFormToObject is what harness.actionPutForm takes (cli.ActionForm).
function actionFormToObject(form) {
  return {
    ref: form.ref,
    revision: form.revision,
    name: form.name,
    description: form.description,
    params: form.params.map((p) => ({ name: p.name, type: p.type, choices: p.type === "choice" ? [...p.choices] : [] })),
    template: form.template.map((e) => (e.isParam
      ? { literal: "", param: e.param, isParam: true }
      : { literal: e.literal, param: "", isParam: false })),
  };
}

// actionFormErrors is why the form cannot be sent yet, by the server's own
// check: [] when it can. Go reports the first rule broken, as the server does.
function actionFormErrors(form) {
  const msg = window.harness.actionFormCheck(actionFormToObject(form));
  return msg ? [msg] : [];
}

// actionFormSetElement makes word i a reference to param name, or a literal
// when name is "". Word 0 is the program and is always literal (C17), so the
// form cannot build the definition the server refuses: false, unchanged.
function actionFormSetElement(form, i, name) {
  if (i < 0 || i >= form.template.length) return false;
  if (name && i === 0) return false;
  form.template[i] = name ? { literal: "", param: name, isParam: true } : { literal: form.template[i].literal || "", param: "", isParam: false };
  return true;
}

// actionRunModel is the run form's choices: per action, a select for a choice
// param and a text box for a token. empty says there is nothing to run, which
// the form shows in words rather than as an empty select.
function actionRunModel(views) {
  const actions = (views || []).map((v) => ({
    id: v.id,
    name: v.name,
    templateText: v.templateText,
    params: (v.params || []).map((p) => (p.type === "choice"
      ? { name: p.name, kind: "select", choices: [...(p.choices || [])] }
      : { name: p.name, kind: "text", choices: [] })),
  }));
  return { empty: actions.length === 0, actions };
}

// actionRunValueSpecs turns the run form's values into exec's NAME=VALUE
// specs, refusing a missing value and a token Go's rule refuses, naming the
// param. error is "" when specs can be sent.
function actionRunValueSpecs(action, values) {
  const specs = [];
  for (const p of action.params) {
    const v = values[p.name];
    if (v === undefined || v === "") return { specs: [], error: `param ${p.name}: no value` };
    if (p.kind === "text" && !window.harness.actionTokenValid(v)) {
      return { specs: [], error: `param ${p.name}: ${JSON.stringify(v)} is not a token (1-128 bytes of [0-9A-Za-z._-], not starting with -)` };
    }
    specs.push(`${p.name}=${v}`);
  }
  return { specs, error: "" };
}

// conditionChecklistModel is an exec_run row's condition as a checklist of
// actions: the condition's ids first, checked, in its order (so serializing an
// untouched checklist returns the condition unchanged), then the other actions
// unchecked. An id the list no longer has is a deleted action: shown as such
// and kept checked until unchecked, so opening a dialog never drops it. cond
// is the row's --grant-condition spelling after the '=' ("actions:<id>,…").
function conditionChecklistModel(views, cond) {
  const ids = (cond || "").startsWith("actions:")
    ? cond.slice("actions:".length).split(",").map((s) => s.trim()).filter(Boolean)
    : [];
  const byId = new Map((views || []).map((v) => [v.id, v]));
  const label = (v) => `${v.name} (${v.id.slice(0, 8)})`;
  const rows = ids.map((id) => (byId.has(id)
    ? { id, label: label(byId.get(id)), checked: true, deleted: false }
    : { id, label: `deleted ${id.slice(0, 8)}`, checked: true, deleted: true }));
  for (const v of views || []) {
    if (!ids.includes(v.id)) rows.push({ id: v.id, label: label(v), checked: false, deleted: false });
  }
  return { empty: rows.length === 0, rows };
}

// conditionFromChecklist is the checked rows in --grant-condition's spelling,
// or "" — no condition — when none is checked.
function conditionFromChecklist(rows) {
  const ids = rows.filter((r) => r.checked).map((r) => r.id);
  return ids.length ? `actions:${ids.join(",")}` : "";
}

// forceStep is what a screen does with a destructive answer (cli.ForceOffer):
// done, or — only on in_use — the second confirmation offering force, which
// lists what the refusal named. The first request is never forced.
function forceStep(offer) {
  if (!offer.needsForce) return { done: true, confirmForce: null };
  const way = (offer.inTheWay || []).map((x) => `  ${x}`).join("\n");
  return { done: false, confirmForce: `${offer.text}\n\nForce it anyway? In the way:\n${way}` };
}

// enrollFormErrors: the key is an Ed25519 public key, 64 hex, as
// `agent-runner --print-pubkey` prints it. The server checks the key itself.
function enrollFormErrors(f) {
  return /^[0-9a-fA-F]{64}$/.test((f.pubkey || "").trim()) ? [] : ["public key: 64 hex characters (agent-runner --print-pubkey)"];
}

// dialFormErrors: a dial record needs at least one candidate and an enrolled
// key — the server refuses a record whose key does not resolve.
function dialFormErrors(f) {
  const errs = [];
  if (!(f.candidates || "").split(",").some((c) => c.trim())) errs.push("at least one candidate address");
  if (!(f.keyPrefix || "").trim()) errs.push("an enrolled key: a runner answering under another key is refused");
  return errs;
}
