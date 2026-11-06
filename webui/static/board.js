"use strict";

// The board panel's pure decisions: composer defaults, send / flush /
// archive / retain result lines and labels.

// boardSendDefaults is what the composer starts from, for a new message
// ({topic}) and a reply ({inReplyTo}) alike: wake on, answers asked for on
// chat.operator. Without a destination an agent's answer is refused
// (no_reply_route), which reads from here as an agent that never answered;
// clearing the field is the opt-out (operator, 2026-10-05).
function boardSendDefaults(_target) {
  // keep = --no-retire-on-reply, off as on the CLI: an answered message is
  // withdrawn so a resumed agent does not redo it.
  return { replyTo: "chat.operator", wake: true, keep: false };
}

// boardMinSeq is the smallest of board seqs given as decimal strings ("" for
// none). Compared as strings, by length and then digit by digit: seqs exceed
// Number.MAX_SAFE_INTEGER, so as numbers neighbours round to one value.
function boardMinSeq(seqs) {
  let min = "";
  for (const s of seqs || []) {
    if (min === "" || s.length < min.length || (s.length === min.length && s < min)) min = s;
  }
  return min;
}

// boardComposeReplyDefault is the composer's reply-to for a new message on
// topic: the topic itself when every subscriber is a member (board join) --
// they read the answers there and may publish there -- else chat.operator.
// One plain subscriber is enough to keep chat.operator: its answer routed to
// the topic would be refused, which reads as an agent that never answered.
function boardComposeReplyDefault(topic, rows) {
  const list = rows || [];
  const all = list.length > 0 && list.every((r) =>
    (r.patterns || []).some((p) => p.name === topic && p.joined));
  return all ? topic : "chat.operator";
}

// boardRowRemoval is a subscriber row's remove button on topic: a member
// leaves (membership and subscription), anyone else is unsubscribed.
function boardRowRemoval(row, topic) {
  const joined = (row.patterns || []).some((p) => p.name === topic && p.joined);
  return joined ? { label: "外す", op: "leave" } : { label: "購読を外す", op: "unsubscribe" };
}

// boardPatternLabel is one subscribed topic in board subscribers' words: a
// member's marked joined:.
function boardPatternLabel(p) {
  return `${p.joined ? "joined:" : ""}${p.name}(shown=${p.shown} pending=${p.pending})`;
}

// boardComposeTarget is where the composer's message goes: a reply names its
// parent only (the server routes it -- the open topic is not necessarily where
// the answer belongs), otherwise the open topic. null when there is neither:
// the chains view has no topic of its own, so there it only replies.
function boardComposeTarget(topic, replyingSeq) {
  if (replyingSeq) return { inReplyTo: String(replyingSeq) };
  return topic ? { topic } : null;
}

// boardSendRequest is what the board composer hands harness.boardSend. Seqs
// stay decimal strings (a board seq exceeds 2^53); "0" means not a reply.
function boardSendRequest({ topic, inReplyTo, replyTo, wake, keep, body }) {
  return {
    topic: topic || "",
    inReplyTo: inReplyTo ? String(inReplyTo) : "0",
    replyTo: replyTo || "",
    noWake: !wake,
    noRetireOnReply: !!keep,
    body,
  };
}

// boardSendResultLine names the target and the change, like every other
// result line on this surface.
function boardSendResultLine(res, req) {
  const dest = req.inReplyTo && req.inReplyTo !== "0"
    ? `as a reply to #${req.inReplyTo}`
    : `to ${req.topic}`;
  return `board send: sent #${res.seq} ${dest} (delivered_to=${res.deliveredTo}, wake ${req.noWake ? "off" : "on"})`;
}

// boardOpenTopicName is the name the open-by-name input asks for: trimmed,
// and null when nothing is left.
function boardOpenTopicName(raw) {
  const name = String(raw || "").trim();
  return name === "" ? null : name;
}

// boardRetainMeta is a topic row's retention text, in board topics' words.
// Empty on a topic that is not retained: there, absence is the answer.
function boardRetainMeta(t) {
  if (!t || !t.retain) return "";
  // until= says when it runs out: the period counts from the later of the
  // last publish and the retain call, so the period alone does not.
  const until = t.retainUntilMs ? ` until=${boardTimeLabel(t.retainUntilMs)}` : "";
  // flushed≤ says where the ring starts: older messages are on disk only.
  const flushed = t.retainFlushed ? ` flushed≤${t.retainFlushed}` : "";
  return `  retain=${t.retain}${until} by=${t.retainBy} disk=${t.diskBytes}${flushed}`;
}

// boardOlderCursor is where "older" pages from: below the oldest seq shown, or
// "0" (the newest on disk) when nothing is shown — a flushed topic's ring is
// empty, and everything it held is on disk.
function boardOlderCursor(seqs) {
  return boardMinSeq(seqs) || "0";
}

// boardFlushResultLine and boardArchiveResultLine name the target and the
// result, in the CLI's words.
function boardFlushResultLine(topic, r) {
  return `board flush: ${topic} through=${r.through} dropped=${r.dropped}`;
}
function boardArchiveResultLine(topic, r) {
  return `board archive: ${topic} → ${r.id} (${r.count} msgs through ${r.through})`;
}

// boardTimeLabel renders a board timestamp (Unix ms) as ISO, or "-" when
// there is none or it cannot be a date. One bad value must not throw: the
// list renderer catches it by replacing EVERY row with the error, which is
// what a server before 84ba1a35 caused with one retained, never-published
// topic (its zero publish time arrived wrapped to ~1.8e19).
function boardTimeLabel(ms) {
  if (!ms) return "-";
  const d = new Date(ms);
  return isNaN(d.getTime()) ? "-" : d.toISOString();
}

// boardRetainButtonLabel says what Retain does on the open topic: set a
// period, or change the one it has.
function boardRetainButtonLabel(retained) {
  return retained ? "⏳ 期間を変更" : "⏳ Retain";
}

// boardRetainResultLine names the target and the change.
function boardRetainResultLine(topic, label) {
  return label === "cleared"
    ? `board retain: ${topic} retention cleared`
    : `board retain: ${topic} retain=${label}`;
}
