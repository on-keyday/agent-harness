package protocol

// Bounds on what a schedule request names (pillar S). A ScheduleSpec carrying
// every field at its bound must fit one control datagram
// (TestScheduleCreateAtItsBoundsFitsOneDatagram); every hop refuses a longer
// one. The prompt is not here: it travels on prompt_stream_id (PromptMax).
const (
	ScheduleCronMax = 256 // bytes of a cron expression, including a TZ= prefix
	ScheduleZoneMax = 64  // bytes of an IANA zone name
	ScheduleHeadMax = 120 // bytes of the prompt head in a ScheduleRow
)

// Where the wire has no bound of its own, a schedule's request takes these so
// that the widest legal create (cron, zone, a u8 agent profile and a conn_id
// selector at their widest, the authority block present) stays in one
// datagram. ScheduleRepoMax is the repo path; ScheduleExtraArgsMax is the total
// bytes of the extra arguments, not counting their length prefixes.
const (
	ScheduleRepoMax      = 128
	ScheduleExtraArgsMax = 64
)
