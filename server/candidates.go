package server

import "github.com/on-keyday/agent-harness/runner/protocol"

// candidatesBody turns the combos of an ambiguous spawn into one row per
// runner connection, in the order the combos arrive (Registry.Candidates'
// order), each row listing the profiles offered on it. Grouping is by
// connection, not identity: during a reconnect takeover two connections can
// briefly share one identity, and each is a separate row.
func candidatesBody(combos []interactiveCombo, repo string) *protocol.RunnerCandidatesBody {
	var rows []protocol.RunnerCandidate
	index := map[string]int{}
	for _, c := range combos {
		key := c.Entry.ID.String()
		i, ok := index[key]
		if !ok {
			var rc protocol.RunnerCandidate
			rc.RunnerId = c.Entry.Identity
			rc.SetCid([]byte(key))
			rc.SetHostname([]byte(c.Entry.Hostname))
			rc.SetMatchedRoot([]byte(matchedRoot(c.Entry.AllowedRoots, repo)))
			rc.ActiveTasks = uint16(len(c.Entry.ActiveTasks))
			rc.MaxTasks = uint16(c.Entry.MaxTasks)
			rows = append(rows, rc)
			i = len(rows) - 1
			index[key] = i
		}
		var name protocol.AgentProfileName
		name.SetName([]byte(c.Profile))
		rows[i].SetProfiles(append(rows[i].Profiles, name))
	}
	var body protocol.RunnerCandidatesBody
	body.SetCandidates(rows)
	return &body
}
