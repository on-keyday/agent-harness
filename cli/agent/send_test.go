package agent

import "testing"

func TestSendTargetArgs_RejectsNeitherTopicNorReply(t *testing.T) {
	if _, err := sendTargetArgs("", 0); err == nil {
		t.Error("err = nil, want an error when neither --topic nor --in-reply-to is given")
	}
}

func TestSendTargetArgs_TopicOnly(t *testing.T) {
	got, err := sendTargetArgs("chat.abcd1234", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "chat.abcd1234" {
		t.Errorf("topic = %q, want %q", got, "chat.abcd1234")
	}
}

func TestSendTargetArgs_ReplyOnlyLeavesTopicEmpty(t *testing.T) {
	got, err := sendTargetArgs("", 99)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("topic = %q, want empty so the server derives it", got)
	}
}
