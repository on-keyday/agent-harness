package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Server-defined actions (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C8, C16, C17).
var (
	ErrActionNotFound  = errors.New("no action has that id")
	ErrActionNameTaken = errors.New("another action has that name")
	ErrStaleRevision   = errors.New("the action was edited since that revision")
)

// ActionStore holds the server-defined actions, persisted as
// <data-dir>/actions.json — the sibling of RunnerKeyStore, and outside the WAL
// for its reason: the store is server configuration. Grants in the WAL name
// action ids, which is why an id is 16 random bytes and never a counter (C16):
// a lost or restored file cannot hand an old id to a new command, and a grant
// naming a lost id reaches nothing.
type ActionStore struct {
	mu      sync.Mutex
	path    string // "" = memory only
	actions map[[16]byte]protocol.ActionRecord
}

// The file holds native JSON fields, never encoded wire bytes, so a later
// .bgn change does not reinterpret it. A template element names its param by
// NAME, which a reader of the file can check against the params above it.
type actionJSON struct {
	ID          string              `json:"id"`
	Revision    uint32              `json:"revision"`
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Params      []actionParamJSON   `json:"params"`
	Template    []actionElementJSON `json:"template"`
	CreatedAtMs uint64              `json:"created_at_ms"`
	EditedAtMs  uint64              `json:"edited_at_ms"`
	CreatedBy   string              `json:"created_by"`
	EditedBy    string              `json:"edited_by"`
}

type actionParamJSON struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"` // "choice" | "token"
	Choices []string `json:"choices,omitempty"`
}

// actionElementJSON is exactly one of the two.
type actionElementJSON struct {
	Literal *string `json:"literal,omitempty"`
	Param   *string `json:"param,omitempty"`
}

// LoadActionStore reads path (absent = empty). A file that does not parse, or
// holds a definition actiondef.Validate refuses, is an error: serving a
// half-read store would run commands nobody defined.
func LoadActionStore(path string) (*ActionStore, error) {
	st := &ActionStore{path: path, actions: map[[16]byte]protocol.ActionRecord{}}
	if path == "" {
		return st, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("actions %s: %w", path, err)
	}
	var rows []actionJSON
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("actions %s: %w", path, err)
	}
	names := map[string]bool{}
	for _, r := range rows {
		rec, err := actionFromJSON(r)
		if err != nil {
			return nil, fmt.Errorf("actions %s: %w", path, err)
		}
		if names[r.Name] {
			return nil, fmt.Errorf("actions %s: two actions are named %q", path, r.Name)
		}
		names[r.Name] = true
		st.actions[rec.Id.Id] = rec
	}
	return st, nil
}

func actionFromJSON(r actionJSON) (protocol.ActionRecord, error) {
	var rec protocol.ActionRecord
	raw, err := hex.DecodeString(r.ID)
	if err != nil || len(raw) != 16 {
		return rec, fmt.Errorf("action %q: id %q is not 32 hex", r.Name, r.ID)
	}
	copy(rec.Id.Id[:], raw)
	rec.Revision, rec.CreatedAtMs, rec.EditedAtMs = r.Revision, r.CreatedAtMs, r.EditedAtMs
	rec.SetCreatedBy([]byte(r.CreatedBy))
	rec.SetEditedBy([]byte(r.EditedBy))
	var d protocol.ActionDefinition
	d.SetName([]byte(r.Name))
	if r.Description != "" {
		d.SetDescription([]byte(r.Description))
	}
	index := map[string]uint8{}
	var params []protocol.ActionParam
	for i, p := range r.Params {
		var ap protocol.ActionParam
		ap.SetName([]byte(p.Name))
		switch p.Type {
		case "token":
			ap.Type = protocol.ActionParamType_Token
		case "choice":
			ap.Type = protocol.ActionParamType_Choice
			var ls []protocol.ActionLiteral
			for _, c := range p.Choices {
				var l protocol.ActionLiteral
				l.SetBytes([]byte(c))
				ls = append(ls, l)
			}
			var ch protocol.ActionChoices
			if !ch.SetChoices(ls) {
				return rec, fmt.Errorf("action %q: param %q has too many choices", r.Name, p.Name)
			}
			ap.SetChoices(ch)
		default:
			return rec, fmt.Errorf("action %q: param %q has type %q", r.Name, p.Name, p.Type)
		}
		index[p.Name] = uint8(i)
		params = append(params, ap)
	}
	if !d.SetParams(params) {
		return rec, fmt.Errorf("action %q: too many params", r.Name)
	}
	var els []protocol.ActionElement
	for i, e := range r.Template {
		switch {
		case e.Literal != nil && e.Param == nil:
			var l protocol.ActionLiteral
			l.SetBytes([]byte(*e.Literal))
			el := protocol.ActionElement{Kind: protocol.ActionElementKind_Literal}
			el.SetLiteral(l)
			els = append(els, el)
		case e.Param != nil && e.Literal == nil:
			pi, ok := index[*e.Param]
			if !ok {
				return rec, fmt.Errorf("action %q: template element %d names no param %q", r.Name, i, *e.Param)
			}
			el := protocol.ActionElement{Kind: protocol.ActionElementKind_Param}
			el.SetParamIndex(pi)
			els = append(els, el)
		default:
			return rec, fmt.Errorf("action %q: template element %d is neither one literal nor one param", r.Name, i)
		}
	}
	if !d.SetTemplate(els) {
		return rec, fmt.Errorf("action %q: template too long", r.Name)
	}
	if err := actiondef.Validate(&d); err != nil {
		return rec, fmt.Errorf("action %q: %w", r.Name, err)
	}
	rec.Definition = d
	return rec, nil
}

func actionToJSON(rec protocol.ActionRecord) actionJSON {
	d := &rec.Definition
	r := actionJSON{
		ID: hex.EncodeToString(rec.Id.Id[:]), Revision: rec.Revision,
		Name: string(d.Name), Description: string(d.Description),
		CreatedAtMs: rec.CreatedAtMs, EditedAtMs: rec.EditedAtMs,
		CreatedBy: string(rec.CreatedBy), EditedBy: string(rec.EditedBy),
		Params: []actionParamJSON{}, Template: []actionElementJSON{},
	}
	for i := range d.Params {
		p := &d.Params[i]
		pj := actionParamJSON{Name: string(p.Name), Type: p.Type.String()}
		if ch := p.Choices(); ch != nil {
			for _, c := range ch.Choices {
				pj.Choices = append(pj.Choices, string(c.Bytes))
			}
		}
		r.Params = append(r.Params, pj)
	}
	for i := range d.Template {
		e := &d.Template[i]
		if l := e.Literal(); l != nil {
			s := string(l.Bytes)
			r.Template = append(r.Template, actionElementJSON{Literal: &s})
		} else if pi := e.ParamIndex(); pi != nil && int(*pi) < len(d.Params) {
			s := string(d.Params[*pi].Name)
			r.Template = append(r.Template, actionElementJSON{Param: &s})
		}
	}
	return r
}

// nameTakenLocked reports whether another action than self holds name.
func (st *ActionStore) nameTakenLocked(name []byte, self [16]byte) bool {
	for id, r := range st.actions {
		if id != self && string(r.Definition.Name) == string(name) {
			return true
		}
	}
	return false
}

// Create stores def under a new random id at revision 1. def is validated by
// the caller (the handler answers invalid_definition with the rule).
func (st *ActionStore) Create(def protocol.ActionDefinition, by string, now time.Time) (protocol.ActionRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.nameTakenLocked(def.Name, [16]byte{}) {
		return protocol.ActionRecord{}, ErrActionNameTaken
	}
	var id [16]byte
	for id == ([16]byte{}) {
		if _, err := rand.Read(id[:]); err != nil {
			return protocol.ActionRecord{}, fmt.Errorf("actions: id: %w", err)
		}
		if _, used := st.actions[id]; used {
			id = [16]byte{}
		}
	}
	ms := uint64(now.UnixMilli())
	rec := protocol.ActionRecord{Id: protocol.ActionID{Id: id}, Revision: 1, Definition: def, CreatedAtMs: ms, EditedAtMs: ms}
	rec.SetCreatedBy([]byte(by))
	rec.SetEditedBy([]byte(by))
	return rec, st.putLocked(rec)
}

// Edit replaces id's definition when its revision is expect, bumping it.
// gate runs under the store lock after the revision and name checks and before
// the write, so a holder check (C11) and the write are one step: two editors
// cannot both pass it against the same revision. A gate error is returned as
// is, and nothing is written.
func (st *ActionStore) Edit(id [16]byte, expect uint32, def protocol.ActionDefinition, by string, now time.Time, gate func() error) (protocol.ActionRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cur, ok := st.actions[id]
	if !ok {
		return protocol.ActionRecord{}, ErrActionNotFound
	}
	if cur.Revision != expect {
		return protocol.ActionRecord{}, ErrStaleRevision
	}
	if st.nameTakenLocked(def.Name, id) {
		return protocol.ActionRecord{}, ErrActionNameTaken
	}
	if gate != nil {
		if err := gate(); err != nil {
			return protocol.ActionRecord{}, err
		}
	}
	next := cur
	next.Revision, next.Definition, next.EditedAtMs = cur.Revision+1, def, uint64(now.UnixMilli())
	next.SetEditedBy([]byte(by))
	return next, st.putLocked(next)
}

// Remove deletes id. A grant naming it then reaches nothing (C7).
func (st *ActionStore) Remove(id [16]byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	cur, ok := st.actions[id]
	if !ok {
		return ErrActionNotFound
	}
	delete(st.actions, id)
	if err := st.saveLocked(); err != nil {
		st.actions[id] = cur
		return err
	}
	return nil
}

// putLocked writes rec, restoring the previous value when the save fails, so
// memory never holds what the file does not.
func (st *ActionStore) putLocked(rec protocol.ActionRecord) error {
	prev, had := st.actions[rec.Id.Id]
	st.actions[rec.Id.Id] = rec
	if err := st.saveLocked(); err != nil {
		if had {
			st.actions[rec.Id.Id] = prev
		} else {
			delete(st.actions, rec.Id.Id)
		}
		return err
	}
	return nil
}

// Get returns id's record.
func (st *ActionStore) Get(id [16]byte) (protocol.ActionRecord, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.actions[id]
	return r, ok
}

// List returns every action sorted by name, so every surface shows one order.
func (st *ActionStore) List() []protocol.ActionRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]protocol.ActionRecord, 0, len(st.actions))
	for _, r := range st.actions {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].Definition.Name) < string(out[j].Definition.Name) })
	return out
}

func (st *ActionStore) saveLocked() error {
	if st.path == "" {
		return nil
	}
	rows := make([]actionJSON, 0, len(st.actions))
	for _, r := range st.actions {
		rows = append(rows, actionToJSON(r))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("actions: %w", err)
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return fmt.Errorf("actions: %w", err)
	}
	return nil
}
