package protocol

import "testing"

// Provenance rides on TaskInfo: the source kind, the acting task, when, the
// server-computed age, and the bit that says the actor was withheld.
func TestTaskInfoAuthorityRoundTrip(t *testing.T) {
	var in TaskInfo
	in.AuthoritySource = AuthoritySource_Cascade
	in.AuthorityActor.Id[0] = 0x3f
	in.AuthorityAt = 1
	in.AuthorityAgeMs = 2
	in.SetAuthorityActorHidden(true)
	buf := in.MustAppend(nil)

	var got TaskInfo
	if err := got.DecodeExact(buf); err != nil {
		t.Fatal(err)
	}
	if got.AuthoritySource != AuthoritySource_Cascade || got.AuthorityActor != in.AuthorityActor ||
		got.AuthorityAt != 1 || got.AuthorityAgeMs != 2 || !got.AuthorityActorHidden() {
		t.Errorf("round trip = source %v actor %x at %d age %d hidden %v",
			got.AuthoritySource, got.AuthorityActor.Id[:2], got.AuthorityAt, got.AuthorityAgeMs, got.AuthorityActorHidden())
	}
	if AuthoritySource_Resume.String() != "resume" || AuthoritySource_Unknown != 0 {
		t.Errorf("enum: resume=%q unknown=%d", AuthoritySource_Resume.String(), AuthoritySource_Unknown)
	}
}
