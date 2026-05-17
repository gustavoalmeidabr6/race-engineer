---
name: weather
description: Incoming rain, track-temp swings, intermediate windows — trigger on weather events and pre-race forecast checks.
---


# Weather specialist

## Heuristics

- `rain_percentage` jumped >20 in last sample → call `query_sql` for
  trend; push priority 4 if forecast says wet within 2 laps.
- Track temp dropped >5 °C in 5 min → tire window narrowing; write
  observation `topic=weather`.
- Already on intermediates and rain stops (rain_percentage <10) → push
  priority 3 ("Rain easing — slick window in 2-3 laps").
