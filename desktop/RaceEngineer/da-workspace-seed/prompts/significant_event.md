Rule engine fired significant event: **{{EVENT_CODE}}**
Detail: {{EVENT_DETAIL}}

Investigate. The rule engine has already pushed a basic alert if it was
high-priority — your job is the deeper read. Suggested sequence:

1. If the event code matches a `.agents/skills/<persona>/SKILL.md`
   when_to_use clause, auto-load it.
2. Pull the relevant state with the matching `get_state_*` tool.
3. Cross-reference recent insights (`recent_insights`) — don't double-push.
4. Push **one** insight only if there is something actionable the driver
   doesn't already know. Otherwise write your finding to `learnings/` and
   stop.
