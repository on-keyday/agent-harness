package transcript

import "errors"

// agy and opencode keep an index rather than one file per session; their
// stores land with Task 3 of the pillar B plan.
var errNotBuilt = errors.New("transcript: store not built yet")

func newAgy(func(string) string) (Store, error)      { return nil, errNotBuilt }
func newOpencode(func(string) string) (Store, error) { return nil, errNotBuilt }
