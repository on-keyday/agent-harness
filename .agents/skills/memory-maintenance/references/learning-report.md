# Learning to hand off

Use this for a discovery that will help later tasks. Omit fields that do not
apply; ordinary progress and a task's to-do list stay in its existing records.
Write one candidate per topic so the maintainer can accept or defer them
independently. Report no candidates when there is nothing durable to retain.

```text
Useful when: the task, subsystem or symptom that should retrieve this note
Claim: what was learned, or what remains a hypothesis
Status: verified | hypothesis | conflict
Evidence: task/session or source document; code path and commit where relevant
Check: command/observation, actual result, date and relevant environment/version
Rationale: why this matters; who made a decision and when, if applicable
Related memory: existing note to update, or the proposed new topic
Re-check when: a concrete source/version/scope change, if known
```

For a bug fix, a passing general test suite does not by itself establish the
cause of the original symptom. State whether the original reproducer passed.
For a user correction or design decision, preserve its source and attribution;
do not present the agent's interpretation as an additional user decision.
Refer to credentials by purpose/name, never by value.
