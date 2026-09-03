package verb

// generatedBuilds maps a verb path to the build the declaration implies. The
// real entries live in actions_gen.go, which cli/verb/gen writes; this file
// declares the map so the package compiles when that output is absent.
//
// That matters because the generator IMPORTS this package -- it reflects over
// the evaluated Verbs rather than parsing table.go, since fifteen verbs build
// their flag lists by calling a function and an AST reader would see none of
// those flags. A package that cannot compile without its own generated file
// cannot generate it, so the map is declared here and FILLED there.
//
//go:generate go run ./gen
var generatedBuilds = map[string]func(Bound) (Action, error){}

// registerGenerated is called from the generated file's init.
func registerGenerated(m map[string]func(Bound) (Action, error)) {
	for k, v := range m {
		generatedBuilds[k] = v
	}
}
