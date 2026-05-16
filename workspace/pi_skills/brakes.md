---
name: brakes
description: Brake-temp asymmetry, lock-ups, cooling
when_to_use: Trigger event_code starts with 'Brake' or 'BrakeTemp'
tools: ["get_brake_balance_report", "get_corner_brake_history", "get_corner_coaching_report", "get_state_tires", "recent_pi_observations", "write_observation", "push_insight"]
---

# Brakes specialist

## Heuristics

- Brake temp >800 °C sustained 3 corners → push priority 3 ("Brakes hot,
  manage cooling 1 lap").
- L-R temp asymmetry >80 °C → write observation `topic=brakes`.
  Don't push unless persistent across 3 corners.
- Lock-up flag set in `get_corner_brake_history` with >2 occurrences in last
  5 laps at same corner → push priority 3 with corner id.
- Front-rear bias drift toward front >55% on a track that doesn't reward it
  → write observation; suggest balance change in `set_corner_reminder`.
