package vtgrid

// OSC 7501, the Program Status Protocol: a program reports its own state
// (idle / working / done / blocked / error) into the terminal stream.
// Spec rev 0.4 (2026-10-10): https://www.superlogical.com/rex/docs/build/program-status
//
// The grammar and limits are ported from stablyai/orca
// src/shared/program-status/protocol.ts (MIT License, Copyright (c) Stably
// AI). Only the ROOT record (no id) is modelled: the harness shows one state
// per task (docs/superpowers/specs/2026-10-27-task-lifecycle-design.md D17).

import (
	"encoding/base64"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ProgramStatus is the root record a program last reported. State and Kind are
// the protocol's words; "" means none.
type ProgramStatus struct {
	State, Kind, Msg string
}

// ProgramStatusQueryReply is the only thing a supporting terminal ever writes
// back: the answer to `OSC 7501 ; ?`, listing what this terminal accepts.
const ProgramStatusQueryReply = "\x1b]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth\x1b\\"

const (
	programStatusMaxSequence = 4096
	programStatusMaxKey      = 16
	programStatusMaxMsgEnc   = 2732
	programStatusMaxMsgDec   = 2048
	programStatusMaxTitleEnc = 256
	programStatusMaxTitleDec = 192
	programStatusMaxApp      = 32
	programStatusMaxID       = 128
	programStatusMaxIDDepth  = 8
)

var (
	psKeyRE     = regexp.MustCompile(`^[a-z]+$`)
	psValueRE   = regexp.MustCompile(`^[A-Za-z0-9_.,+/=-]*$`)
	psSegmentRE = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,32}$`)
)

func programStatusState(s string) bool {
	switch s {
	case "idle", "working", "done", "blocked", "error":
		return true
	}
	return false
}

func programStatusKind(s string) bool {
	switch s {
	case "permission", "question", "auth":
		return true
	}
	return false
}

func validProgramStatusID(id string) bool {
	if len(id) > programStatusMaxID {
		return false
	}
	segs := strings.Split(id, "/")
	if len(segs) > programStatusMaxIDDepth {
		return false
	}
	for _, s := range segs {
		if !psSegmentRE.MatchString(s) {
			return false
		}
	}
	return true
}

// decodeProgramStatusText decodes a base64 msg/title, or reports false when
// the whole report must be discarded. The encoded cap is checked before
// decoding: it bounds the work a hostile program can cause.
func decodeProgramStatusText(enc string, maxEnc, maxDec int) (string, bool) {
	if len(enc) > maxEnc {
		return "", false
	}
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(enc, "="))
	if err != nil || len(b) > maxDec || !utf8.Valid(b) {
		return "", false
	}
	for _, r := range string(b) {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return "", false
		}
	}
	return string(b), true
}

// ParseProgramStatus parses the body between `OSC 7501;` and ST. query is true
// for the probe `?`. ok is false when the report must be ignored: no or an
// unknown state, a child record (id present), or a broken hard limit. A
// malformed pair is skipped and the rest of the report still applies; a
// repeated key's last value wins, but every occurrence is checked against the
// caps, so a later duplicate cannot launder an over-limit one.
func ParseProgramStatus(body string) (st ProgramStatus, query bool, ok bool) {
	if len(body) > programStatusMaxSequence {
		return ProgramStatus{}, false, false
	}
	if strings.TrimSpace(body) == "?" {
		return ProgramStatus{}, true, false
	}
	pairs := map[string]string{}
	var msg string
	for _, pair := range strings.Split(body, ":") {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		key, value := strings.TrimSpace(pair[:eq]), strings.TrimSpace(pair[eq+1:])
		if len(key) > programStatusMaxKey ||
			(key == "app" && len(value) > programStatusMaxApp) ||
			(key == "id" && !validProgramStatusID(value)) {
			return ProgramStatus{}, false, false
		}
		if !psKeyRE.MatchString(key) || !psValueRE.MatchString(value) {
			continue
		}
		switch key {
		case "msg":
			text, good := decodeProgramStatusText(value, programStatusMaxMsgEnc, programStatusMaxMsgDec)
			if !good {
				return ProgramStatus{}, false, false
			}
			msg = text
			continue
		case "title":
			// Not shown by the harness, but its limits still void the report.
			if _, good := decodeProgramStatusText(value, programStatusMaxTitleEnc, programStatusMaxTitleDec); !good {
				return ProgramStatus{}, false, false
			}
			continue
		}
		pairs[key] = value
	}
	if _, child := pairs["id"]; child {
		return ProgramStatus{}, false, false
	}
	state, has := pairs["state"]
	if !has {
		return ProgramStatus{}, false, false
	}
	if state == "clear" {
		return ProgramStatus{}, false, true
	}
	if !programStatusState(state) {
		return ProgramStatus{}, false, false
	}
	st.State = state
	if k := pairs["kind"]; state == "blocked" && programStatusKind(k) {
		st.Kind = k
	}
	st.Msg = msg
	return st, false, true
}
