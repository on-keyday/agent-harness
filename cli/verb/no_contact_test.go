package verb

import "testing"

// --no-contact is agent send's (reach spec D17). dispatch blocks for the reply
// that the flag exists not to open a route for, so it must refuse the flag
// rather than accept and contradict it.
func TestNoContactIsOnlyOnAgentSend(t *testing.T) {
	v, _ := Lookup("agent", "send")
	v = v.For(CLI)
	b, err := v.Parse(v.NewFlagSet(0), []string{"--topic", "chat.abcd1234", "--no-contact", "hello"})
	if err != nil {
		t.Fatalf("agent send --no-contact: %v", err)
	}
	act, err := v.BuildFunc()(b)
	if err != nil {
		t.Fatal(err)
	}
	if !act.(AgentSendAction).NoContact {
		t.Error("agent send --no-contact did not set NoContact")
	}

	d, _ := Lookup("agent", "dispatch")
	d = d.For(CLI)
	if _, err := d.Parse(d.NewFlagSet(0), []string{"--topic", "chat.abcd1234", "--no-contact", "hello"}); err == nil {
		t.Error("agent dispatch accepted --no-contact")
	}
}
