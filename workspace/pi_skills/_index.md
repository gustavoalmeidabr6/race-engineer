---
name: _index
description: How the pi-agent skill system works
---

# Pi Agent Skills

Each `*.md` in this folder is a playbook the pi-agent's specialists load on
demand via the MCP `get_skill` tool. The frontmatter (name, description,
when_to_use) is what `list_skills` returns; the body is what the model reads
once it picks a skill.

To add a new skill, drop a new `*.md` here with the same shape. No code
changes required — the registry rescans on every `list_skills` call.

Specialists currently shipped:

- `tires` — tire wear, compound choice, cliff detection
- `brakes` — brake-temp asymmetry, lock-ups, cooling
- `pace` — sector deltas, lap-time trends, session-best comparison
- `strategy` — pit windows, undercut/overcut, weather-driven changes
- `weather` — incoming rain, track-temp swings, intermediate windows
- `energy` — fuel target, ERS deployment, harvest balance
- `driving` — driver inputs (smoothness, throttle/brake overlap, gear use)
- `responder` — reply to /api/analyst/query in ≤3 sentences
