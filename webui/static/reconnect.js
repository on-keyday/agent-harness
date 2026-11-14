// Waking up after the phone slept. A sleeping page gets no close event for a
// connection the network dropped, and the persist loop's backoff keeps growing
// while the radio is off; this file is the page's side of getting back sooner.
// Plain declarations (no DOM reach at load) so reconnect_test.mjs can drive it.

// A live-connection probe is given this long to answer before the page calls
// the connection dead and lets the persist loop dial again.
const WAKE_PROBE_MS = 3000;

// wakeReconnect acts on one wake event. Down: end the persist loop's backoff
// sleep at once. Up: the connection may be dead without any event saying so,
// so ask the bridge to prove it is alive (it closes the connection itself when
// the answer does not come).
async function wakeReconnect(harness, isUp, probeMs) {
  if (!isUp()) {
    harness.reconnectNow();
    return "kick";
  }
  const alive = await harness.probeConnection(probeMs);
  return alive ? "alive" : "dead";
}

// installWakeReconnect wires the three events a returning phone raises:
// visibilitychange to visible, `online`, and a back/forward-cache `pageshow`.
// Visibility is also told to the bridge, which caps the backoff low while the
// page is visible and lets it grow while hidden.
function installWakeReconnect(win, doc, harness, isUp) {
  const wake = () => {
    wakeReconnect(harness, isUp, WAKE_PROBE_MS).catch((e) => console.error("wake reconnect", e));
  };
  harness.setPageVisible(!doc.hidden);
  doc.addEventListener("visibilitychange", () => {
    harness.setPageVisible(!doc.hidden);
    if (!doc.hidden) wake();
  });
  win.addEventListener("online", wake);
  win.addEventListener("pageshow", (e) => { if (e && e.persisted) wake(); });
}

// previewStartFailure turns a rejected harness.previewStart into the note the
// pane shows. A timeout (the bridge's deadline) means the connection did not
// answer, which is not the session's fault: the pane says it is waiting and
// resumePanes restarts it after the next 'connected'.
function previewStartFailure(err) {
  const msg = (err && err.message) || String(err);
  if (err && err.code === "timeout") {
    return { waiting: true, text: "接続待ち — 再接続後に自動で再開します" };
  }
  return { waiting: false, text: `preview error: ${msg}` };
}

// resumePanes restarts every pane the operator still wants live. After a
// reconnect the old connection's streams are gone, so a pane that still reads
// live, one whose stream died with the connection, and one whose start timed
// out all need a fresh start. Panes the operator paused (wantLive false) stay
// paused. A pane whose session had ended also restarts once and shows the
// ordinary preview error, which is cheaper than guessing why a stream closed.
function resumePanes(panes, start) {
  for (const [key, p] of panes) {
    if (p.wantLive) start(key, p);
  }
}

// retryTimedOutStart runs after a pane's start timed out. If the connection
// answers a probe it was only slow (or came back without a reconnect, so no
// 'connected' will fire): start the pane again. If it does not answer, the
// probe has already closed it and the persist loop's next 'connected' restarts
// the pane through resumePanes. Gives up after 3 retries — a link that answers
// probes while the attach keeps timing out is the server's problem.
async function retryTimedOutStart(harness, panes, key, p, epoch, start) {
  const alive = await harness.probeConnection(WAKE_PROBE_MS);
  if (epoch !== p.epoch || panes.get(key) !== p || !p.wantLive) return "stale";
  if (!alive) return "left-to-reconnect";
  p.timeoutRetries = (p.timeoutRetries || 0) + 1;
  if (p.timeoutRetries > 3) return "gave-up";
  start(key, p);
  return "retried";
}
