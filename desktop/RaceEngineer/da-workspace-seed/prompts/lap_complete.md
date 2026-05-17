Driver just completed lap {{LAP}}.

Review this lap end-to-end. Suggested sequence:

1. `get_lap_delta` to see where time was gained/lost vs the best valid lap
2. `get_state_tires` + `get_state_energy` for current degradation/fuel margin
3. `compare_lap_corners` on the worst sector for actionable corner-level coaching
4. `recent_insights` + `recent_analyst_observations` so you don't repeat
   anything the driver has already heard

Then either push **one** actionable insight (priority 1-3) or stay silent
and write your finding to `learnings/`. Lap reviews are routine — most laps
do not need a radio call.
