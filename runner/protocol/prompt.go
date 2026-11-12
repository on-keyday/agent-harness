package protocol

// PromptMax is the most a session's initial prompt may hold, in bytes, on
// every leg (client→server and server→runner) and for both session kinds.
// Sized so the stream kind's adapter argument (--prompt) fits Windows'
// 32,767-character command line with room for the rest of the argv (pillar A
// spec, A-D17).
const PromptMax = 16 << 10
