package protocol

// PromptMax is the most a session's initial prompt may hold, in bytes, on
// every leg (client→server and server→runner) and for both session kinds
// (pillar A spec, A-D17). Sized so typical text fits the stream kind's adapter
// argument (--prompt) inside Windows' 32,767-character command line. Not every
// prompt does: Windows argv quoting doubles each '"', and a .cmd/.bat agent goes
// through cmd.exe's shorter limit. Those fail loudly — the runner's spawn
// errors and the task ends Failed — never by silent truncation.
const PromptMax = 16 << 10
