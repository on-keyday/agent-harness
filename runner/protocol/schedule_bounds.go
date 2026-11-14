package protocol

// Bounds on what a schedule request names (pillar S). The cron expression and
// the zone name are bounded here; the server refuses a longer one. The rest of
// a request (repo path, extra args, scope ids) has no bound of its own: as with
// OpenInteractiveRequest, the guard is the client's CheckControlMessage, which
// refuses a request over the connection's datagram budget before sending it.
// The prompt is not here: it travels on prompt_stream_id (PromptMax).
const (
	ScheduleCronMax = 256 // bytes of a cron expression, including a TZ= prefix
	ScheduleZoneMax = 64  // bytes of an IANA zone name
	ScheduleHeadMax = 120 // bytes of the prompt head in a ScheduleRow
)
