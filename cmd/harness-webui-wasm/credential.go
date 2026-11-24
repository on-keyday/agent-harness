//go:build js

package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"syscall/js"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Credential tasks on the WebUI (docs/superpowers/specs/2026-11-21-credential-
// tasks-design.md). Every call reaches cli through the page's long-lived
// client (currentClient, pitfall 3) and the cli functions the CLI and the TUI
// use, so an issue from the dialog, the command line or `harness-cli` is the
// same request and reads the same words.
//
// credentialNew / credentialRekey take the verb's own argument line: the
// command line passes what was typed and the dialog serializes its fields to
// the same grammar (credentialIssueTokens), so the parser stays in Go. The
// line is parsed BEFORE the client is asked for, so a malformed one is
// refused with the grammar's words whether or not a server is up.
//
// The secret of a generated key pair is the result's `secret` field and
// nothing else: the pair is generated here, in the page's own process, and
// only its public half is sent. `text` never carries it, because text is what
// the command output prints; the page shows `secret` once in the Credentials
// dialog and drops it when the dialog closes.

// credentialPromise is schedulePromise for the credential verbs, whose cli
// errors already read `credential <verb>: <cause>`.
func credentialPromise(parse func() (any, error), fn func(c *cli.Client, parsed any) (any, error)) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve, reject := promiseArgs[0], promiseArgs[1]
		go func() {
			parsed, err := parse()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, fmt.Errorf("credential: %w", err))
				return
			}
			v, err := fn(c, parsed)
			if err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessCredentialNew(tokens) → Promise<{id, text, expiresAt, secret?}>:
// `credential new <tokens>`.
func harnessCredentialNew(this js.Value, args []js.Value) any {
	return credentialIssuePromise(args, false)
}

// harnessCredentialRekey(tokens) → the same, for `credential rekey <tokens>`
// (the credential id first).
func harnessCredentialRekey(this js.Value, args []js.Value) any {
	return credentialIssuePromise(args, true)
}

func credentialIssuePromise(args []js.Value, rekey bool) any {
	var tokens []string
	if len(args) > 0 {
		tokens = commandFields(args[0])
	}
	parse := verb.ParseCmdCredentialNew
	if rekey {
		parse = verb.ParseCmdCredentialRekey
	}
	return credentialPromise(func() (any, error) {
		// The parser's errors already name the verb.
		a, err := parse(verb.WebUI, tokens, nil)
		if err != nil {
			return nil, err
		}
		return a, nil
	}, func(c *cli.Client, parsed any) (any, error) {
		h, err := cli.IssueCredential(rootCtx, c, parsed.(verb.CredentialIssueAction))
		if err != nil {
			return nil, err
		}
		id := hex.EncodeToString(h.TaskID.Id[:])
		text := cli.CredentialIssuedLine(h.CredentialIssued)
		res := map[string]any{
			"id":        id,
			"expiresAt": float64(h.ExpiresAt.UnixNano()),
			"expires":   h.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if h.Cred != nil {
			res["secret"] = h.Cred.Secret()
		} else {
			text += "\n" + cli.CredentialBindHint(id)
		}
		res["text"] = text
		return res, nil
	})
}

// credentialRowJS is one `credential ls` row as the dialog reads it: the cli
// labels (`unknown` for a last use the server has not seen since it started)
// beside the raw times, and line, what the command line prints.
func credentialRowJS(t protocol.TaskInfo) map[string]any {
	return map[string]any{
		"id":         hex.EncodeToString(t.Id.Id[:]),
		"status":     t.Status.String(),
		"name":       string(t.Name),
		"expires":    cli.CredentialExpiryLabel(&t),
		"expiresAt":  float64(t.CredentialExpiresAt),
		"lastUsed":   cli.CredentialLastUsedLabel(&t),
		"lastUsedAt": float64(t.CredentialLastUsedAt),
		"caps":       cli.CapsLabel(t.Capabilities),
		"scope":      cli.ScopeLabel(t.Scope),
		"line":       cli.CredentialRowLine(t),
	}
}

// harnessCredentialList({json}) → Promise<{rows, text}>: text is what
// `credential ls` (or --json) prints, rows the same list for the dialog.
func harnessCredentialList(this js.Value, args []js.Value) any {
	asJSON := len(args) > 0 && args[0].Type() == js.TypeObject && args[0].Get("json").Truthy()
	return credentialPromise(func() (any, error) { return nil, nil }, func(c *cli.Client, _ any) (any, error) {
		lr, err := c.SnapshotFinished(rootCtx, verb.FinishedAll) // as credential ls: ended ones too
		if err != nil {
			return nil, err
		}
		rows := make([]any, 0)
		for _, t := range cli.CredentialRows(lr.Tasks) {
			rows = append(rows, credentialRowJS(t))
		}
		var text bytes.Buffer
		if asJSON {
			if err := cli.CredentialListJSON(&text, lr.Tasks); err != nil {
				return nil, err
			}
		} else {
			cli.CredentialListText(&text, lr.Tasks)
		}
		return map[string]any{"rows": rows, "text": strings.TrimRight(text.String(), "\n")}, nil
	})
}

// harnessCredentialRevoke(id) → Promise<string>: the line `credential revoke`
// prints, naming the credential and what its end does.
func harnessCredentialRevoke(this js.Value, args []js.Value) any {
	id := argString(args, 0)
	return credentialPromise(func() (any, error) { return nil, nil }, func(c *cli.Client, _ any) (any, error) {
		var out bytes.Buffer
		if err := cli.CredentialRevokeWith(rootCtx, c, id, &out); err != nil {
			return nil, err
		}
		return strings.TrimRight(out.String(), "\n"), nil
	})
}
