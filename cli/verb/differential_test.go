package verb

import (
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// legacyPrune is cmd/harness-cli/main.go's prune parser as it stood before the
// migration. It is now the only copy: all three surfaces parse prune from the
// declaration, so this is a hand-written reference implementation rather than
// a differential against live code.
//
// Kept deliberately, and worth knowing the scope of: prune, `file push` and
// `file pull` are the THREE paths of eighty that ever got a differential. Every
// other family's parser was deleted with no such proof, which is how the drops
// this file's siblings now pin -- exec ls's filter, --x11-display's default,
// approve's suggestion index -- shipped green.
func legacyPrune(args []string) (time.Duration, []string, bool, error) {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	before := fs.Duration("before", 7*24*time.Hour, "")
	force := fs.Bool("force", false, "")
	fs.BoolVar(force, "f", false, "")
	taskIDs, err := ParsePermuted(fs, args)
	if err != nil {
		return 0, nil, false, err
	}
	return *before, taskIDs, *force, nil
}

// pruneCorpus is every prune form the three surfaces document or test:
// tui/cmdline_test.go supplies the first five, the CLI's usage line the rest.
var pruneCorpus = [][]string{
	// The bare form is deliberately absent: the legacy parser accepted it and
	// the declaration refuses it now. `prune` with no ids and no cutoff forgot
	// every terminal task older than the default, deleting each TaskEntry and
	// its log -- after which `submit --resume <id>` answers resume_not_found.
	// This test asserts the migration changed NO behaviour, so the one place
	// it deliberately did says so here.
	{"--before=1h"},
	{"--before", "1h"},
	{"--force", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	{"-f", "deadbeefdeadbeefdeadbeefdeadbeef"},
	{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--force"},
	{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	{"--before", "30m", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
}

// TestPruneDeclarationMatchesLegacy is the proof that this phase changed no
// behaviour. It is the gate on deleting a hand-written parser, and the pattern
// every later phase repeats: a legacy parser and the declaration, fed the same
// corpus, asserted to agree.
func TestPruneDeclarationMatchesLegacy(t *testing.T) {
	sp, ok := Lookup("prune")
	if !ok {
		t.Fatal("prune is not in the table")
	}
	for _, args := range pruneCorpus {
		wantBefore, wantIDs, wantForce, wantErr := legacyPrune(args)

		fs := sp.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		b, err := sp.Parse(fs, args)
		if (err != nil) != (wantErr != nil) {
			t.Errorf("%q: err = %v, legacy err = %v", args, err, wantErr)
			continue
		}
		if err != nil {
			continue
		}
		act, berr := sp.BuildFunc()(b)
		if berr != nil {
			t.Errorf("%q: Build: %v", args, berr)
			continue
		}
		got, isPrune := act.(PruneAction)
		if !isPrune {
			t.Errorf("%q: Build returned %T, want PruneAction", args, act)
			continue
		}
		if got.Before != wantBefore {
			t.Errorf("%q: Before = %v, legacy %v", args, got.Before, wantBefore)
		}
		if got.Force != wantForce {
			t.Errorf("%q: Force = %v, legacy %v", args, got.Force, wantForce)
		}
		if !reflect.DeepEqual(nilAsEmpty(got.TaskIDs), nilAsEmpty(wantIDs)) {
			t.Errorf("%q: TaskIDs = %q, legacy %q", args, got.TaskIDs, wantIDs)
		}
	}
}

func nilAsEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// TestExamplesParse is not cosmetic. The board purge incident was a usage line
// the parser rejected, and Usage() is generated from this same spec -- so an
// example that fails to parse is a documented invocation that does not work.
func TestExamplesParse(t *testing.T) {
	for _, v := range Verbs {
		for _, ex := range v.Examples {
			args := splitExample(ex, v)
			fs := v.NewFlagSet(flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			b, err := v.Parse(fs, args)
			if err != nil {
				t.Errorf("%s: example %q does not parse: %v\n%s",
					v.FlagSetName(), ex, err, v.Usage())
				continue
			}
			if _, err := v.BuildFunc()(b); err != nil {
				t.Errorf("%s: example %q parses but does not build: %v", v.FlagSetName(), ex, err)
			}
		}
	}
}

// splitExample drops the verb path words from the front of an example and
// returns the argument tail.
func splitExample(ex string, v VerbSpec) []string {
	fields := strings.Fields(ex)
	if len(fields) < len(v.Path) {
		return nil
	}
	return fields[len(v.Path):]
}
