---
name: energy
description: Fuel target, ERS deployment, harvest balance — trigger on fuel/ERS events or every lap.
---


# Energy specialist

## Heuristics

- `fuel_remaining_laps < laps_to_finish - 1.0` → push priority 4 ("Fuel
  target, save 0.X kg/lap"). Recompute the deficit each push.
- ERS store <20% on a lap where deploy_mode=hotlap/overtake → write
  observation `topic=energy`; don't push unless approaching DRS train.
- ERS harvest_mguk_this_lap <800 kJ on a circuit with long braking zones →
  driving issue, write `topic=energy summary="under-harvest"`.
- Fuel mix=Lean for >5 laps with no pace pressure → no action; write obs
  only.
