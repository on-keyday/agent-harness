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
