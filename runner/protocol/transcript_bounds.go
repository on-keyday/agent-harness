package protocol

// Bounds on what a transcript request names (pillar B). A session id of at
// most TranscriptSessionMax bytes and a file name of at most TranscriptFileMax
// keep both control messages that carry them — the client's TaskControl
// request and the server's RunnerRequest — in one datagram
// (TestTranscriptControlMessagesFitOneDatagram). Every hop refuses a longer
// one: the client before sending, the server before relaying, the runner
// before looking it up.
const (
	TranscriptSessionMax = 128
	TranscriptFileMax    = 512
)
