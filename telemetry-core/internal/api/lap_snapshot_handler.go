package api

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/trackmap"
)

// lap_snapshot_handler.go renders a single lap as a compact ASCII snapshot
// the LLM can read directly. Same data as /api/laps/traces + /api/laps/delta
// but rendered as text strip-charts so the model gets a "glance at the
// telemetry" view without us having to wire up an image pipeline.
//
//	GET /api/laps/snapshot
//	  ?lap=N|last                   (default: last)
//	  ?reference=best|N|none        (default: best — also computes delta strip)
//	  ?width=80                     (40..160; controls bucket count and chart width)
//	  ?channels=throttle,brake,...  (optional override; default set below)
//	  ?session_uid=…                (optional past session for the lap)
//	  ?reference_session_uid=…      (optional past session for the reference)
//
// Returns Content-Type: text/plain so the model sees the rendered output
// verbatim. JSON variant could be added later — text is what unlocks the
// "model glances at the lap" use case the user described.

// snapshotChannel is one strip rendered in the snapshot. style picks the
// glyph palette: "analog" uses block heights, "gear" prints the integer
// gear, "surface" prints a category letter, "delta" centres around zero.
type snapshotChannel struct {
	id     string
	label  string
	column string
	style  string // analog | gear | surface | delta
	// minV/maxV are display clamps for analog channels. Zero means auto.
	minV, maxV float64
}

var defaultSnapshotChannels = []snapshotChannel{
	{id: "throttle", label: "throttle", column: "throttle", style: "analog", minV: 0, maxV: 1},
	{id: "brake", label: "brake   ", column: "brake", style: "analog", minV: 0, maxV: 1},
	{id: "speed", label: "speed   ", column: "speed", style: "analog"},
	{id: "gear", label: "gear    ", column: "gear", style: "gear"},
	{id: "slip", label: "slip(R) ", column: "(ABS(wheel_slip_ratio_fl)+ABS(wheel_slip_ratio_fr)+ABS(wheel_slip_ratio_rl)+ABS(wheel_slip_ratio_rr))/4.0", style: "analog"},
	{id: "drs", label: "DRS     ", column: "drs", style: "gear"},
	{id: "surface", label: "surface ", column: "GREATEST(surface_type_fl, surface_type_fr, surface_type_rl, surface_type_rr)", style: "surface"},
}

func lapSnapshotHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).SendString("error: " + err.Error())
		}
		trackLen := float64(state.trackLength)
		uid := state.sessionUID
		carIdx := state.playerCarIndex

		width := parseIntDefault(c.Query("width"), 80)
		if width < 40 {
			width = 40
		}
		if width > 160 {
			width = 160
		}
		bucketSize := trackLen / float64(width)

		// Cross-session overrides — same contract as /api/laps/delta so a
		// caller can chain best_at_track → snapshot directly.
		yourUID, yourCar, err := resolveCrossSession(c, deps, "lap_session_uid", uid, carIdx, trackLen)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).SendString("error: " + err.Error())
		}
		refUID, refCar, err := resolveCrossSession(c, deps, "reference_session_uid", uid, carIdx, trackLen)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).SendString("error: " + err.Error())
		}

		your, yourMs, err := resolveSingleLap(c, deps, yourUID, yourCar, c.Query("lap"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).SendString("error: " + err.Error())
		}
		if your == 0 {
			return c.Status(fiber.StatusNotFound).SendString("no completed laps in this session yet")
		}

		// Reference lap is optional. ?reference=none skips the delta strip
		// (useful for "show me this lap on its own" output).
		refRaw := strings.ToLower(strings.TrimSpace(c.Query("reference", "best")))
		var (
			ref   int
			refMs int
		)
		if refRaw != "none" {
			ref, refMs, err = resolveReferenceLap(c, deps, refUID, refCar, refRaw)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).SendString("error: " + err.Error())
			}
		}

		channels := defaultSnapshotChannels
		if raw := strings.TrimSpace(c.Query("channels")); raw != "" {
			channels = filterSnapshotChannels(raw)
		}

		// Build the channel SELECT list. Each channel produces an AVG()
		// per bucket per (session, lap). Surface uses GREATEST so off-track
		// moments still flash through.
		selectExprs := make([]string, 0, len(channels)+1)
		for _, ch := range channels {
			agg := "AVG"
			if ch.style == "surface" {
				agg = "MAX"
			}
			selectExprs = append(selectExprs, fmt.Sprintf("%s(%s) AS %s", agg, ch.column, ch.id))
		}
		// Time bucket for the delta strip — pull always; cheap.
		selectExprs = append(selectExprs, "AVG(current_lap_time_ms) AS t_ms")

		var keyOR []string
		keyOR = append(keyOR, fmt.Sprintf("(session_uid = %s AND lap = %d)", uidString(yourUID), your))
		if ref > 0 {
			keyOR = append(keyOR, fmt.Sprintf("(session_uid = %s AND lap = %d)", uidString(refUID), ref))
		}
		sql := fmt.Sprintf(`
SELECT session_uid, lap,
       FLOOR(track_position / %f)::INT AS bucket,
       %s
FROM telemetry_hifreq
WHERE (%s)
  AND pit_status = 0
  AND track_position IS NOT NULL
  AND track_position >= 0
GROUP BY session_uid, lap, bucket
ORDER BY session_uid, lap, bucket`, bucketSize, strings.Join(selectExprs, ", "), strings.Join(keyOR, " OR "))

		rows, err := deps.Store.Query(c.Context(), sql)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("error: " + err.Error())
		}

		// Slice per (lap, channel). Pointer floats so empty buckets stay
		// distinguishable from "0.0".
		makeArr := func() []*float64 { return make([]*float64, width) }
		yourArrs := map[string][]*float64{}
		refArrs := map[string][]*float64{}
		for _, ch := range channels {
			yourArrs[ch.id] = makeArr()
			if ref > 0 {
				refArrs[ch.id] = makeArr()
			}
		}
		yourT := makeArr()
		refT := makeArr()

		for _, row := range rows {
			b := toInt(row["bucket"])
			if b < 0 || b >= width {
				continue
			}
			lap := toInt(row["lap"])
			rowUID := toUint64(row["session_uid"])
			isYour := rowUID == yourUID && lap == your
			isRef := ref > 0 && rowUID == refUID && lap == ref
			if !isYour && !isRef {
				continue
			}
			for _, ch := range channels {
				v := row[ch.id]
				if v == nil {
					continue
				}
				f := toFloat(v)
				if isYour {
					yourArrs[ch.id][b] = &f
				} else {
					refArrs[ch.id][b] = &f
				}
			}
			if v := row["t_ms"]; v != nil {
				f := toFloat(v)
				switch {
				case isYour:
					yourT[b] = &f
				case isRef:
					refT[b] = &f
				}
			}
		}

		// ----- header -----
		var b strings.Builder
		fmt.Fprintf(&b, "Track: %s (id=%d) length=%.0fm\n", enums.Track(state.trackID), state.trackID, trackLen)
		fmt.Fprintf(&b, "Lap %d %s", your, msToLap(yourMs))
		if yourUID != uid {
			fmt.Fprintf(&b, " [session %d]", yourUID)
		}
		if ref > 0 {
			fmt.Fprintf(&b, "  |  ref lap %d %s", ref, msToLap(refMs))
			if refUID != uid {
				fmt.Fprintf(&b, " [session %d]", refUID)
			}
			if yourMs > 0 && refMs > 0 {
				delta := yourMs - refMs
				sign := "+"
				if delta < 0 {
					sign = ""
				}
				fmt.Fprintf(&b, "  |  Δ %s%s", sign, msToDelta(delta))
			}
		}
		b.WriteString("\n\n")

		// ----- strip charts -----
		for _, ch := range channels {
			b.WriteString(ch.label + " ")
			b.WriteString(renderStrip(yourArrs[ch.id], ch))
			b.WriteByte('\n')
		}
		if ref > 0 {
			b.WriteString("\n--- reference lap ---\n")
			for _, ch := range channels {
				b.WriteString(ch.label + " ")
				b.WriteString(renderStrip(refArrs[ch.id], ch))
				b.WriteByte('\n')
			}

			// Delta strip — cumulative ms behind/ahead at each bucket.
			b.WriteString("\ndelta(ms)")
			b.WriteByte(' ')
			b.WriteString(renderDelta(yourT, refT))
			b.WriteByte('\n')
		}

		// ----- corner labels along the bottom -----
		if corners := lookupCorners(deps.TrackMap, int8(state.trackID)); len(corners) > 0 {
			b.WriteString("\n")
			b.WriteString(renderCornerRuler(corners, trackLen, width))
			b.WriteByte('\n')
		}

		// ----- surface legend if rendered -----
		for _, ch := range channels {
			if ch.style == "surface" {
				b.WriteString("\nsurface legend: . tarmac, k kerb, g grass, G gravel, x off-track\n")
				break
			}
		}

		c.Type("txt", "utf-8")
		return c.SendString(b.String())
	}
}

func filterSnapshotChannels(raw string) []snapshotChannel {
	want := map[string]bool{}
	for _, t := range strings.Split(raw, ",") {
		want[strings.ToLower(strings.TrimSpace(t))] = true
	}
	out := make([]snapshotChannel, 0, len(defaultSnapshotChannels))
	for _, ch := range defaultSnapshotChannels {
		if want[ch.id] {
			out = append(out, ch)
		}
	}
	if len(out) == 0 {
		return defaultSnapshotChannels
	}
	return out
}

// renderStrip turns a per-bucket float slice into a single line of glyphs.
func renderStrip(arr []*float64, ch snapshotChannel) string {
	switch ch.style {
	case "gear":
		return renderGearStrip(arr)
	case "surface":
		return renderSurfaceStrip(arr)
	default:
		return renderAnalogStrip(arr, ch.minV, ch.maxV)
	}
}

var analogGlyphs = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

func renderAnalogStrip(arr []*float64, lo, hi float64) string {
	mn, mx := lo, hi
	if mn == 0 && mx == 0 {
		mn = math.Inf(1)
		mx = math.Inf(-1)
		for _, v := range arr {
			if v == nil {
				continue
			}
			if *v < mn {
				mn = *v
			}
			if *v > mx {
				mx = *v
			}
		}
		if math.IsInf(mn, 1) || mx == mn {
			return strings.Repeat(" ", len(arr))
		}
	}
	span := mx - mn
	out := make([]rune, len(arr))
	for i, v := range arr {
		if v == nil {
			out[i] = ' '
			continue
		}
		x := (*v - mn) / span
		if x < 0 {
			x = 0
		} else if x > 1 {
			x = 1
		}
		idx := int(x * float64(len(analogGlyphs)-1))
		out[i] = analogGlyphs[idx]
	}
	return string(out)
}

func renderGearStrip(arr []*float64) string {
	out := make([]rune, len(arr))
	for i, v := range arr {
		if v == nil {
			out[i] = ' '
			continue
		}
		g := int(math.Round(*v))
		switch {
		case g <= 0:
			out[i] = 'N'
		case g >= 1 && g <= 9:
			out[i] = rune('0' + g)
		default:
			out[i] = '+'
		}
	}
	return string(out)
}

// renderSurfaceStrip maps the F1 25 surface enum to a single legend char.
// 0=tarmac (.), 1=rumblestrip (k), 4=gravel (G), 7=grass (g), other=x.
func renderSurfaceStrip(arr []*float64) string {
	out := make([]rune, len(arr))
	for i, v := range arr {
		if v == nil {
			out[i] = ' '
			continue
		}
		switch int(math.Round(*v)) {
		case 0:
			out[i] = '.'
		case 1:
			out[i] = 'k'
		case 7:
			out[i] = 'g'
		case 4:
			out[i] = 'G'
		default:
			out[i] = 'x'
		}
	}
	return string(out)
}

// renderDelta produces a strip centered on zero — '-' means you're faster,
// '+' means you're slower. Magnitude is rendered as block height above /
// below the centre line via two-character cells: top half for negative,
// bottom half for positive. Keeps the strip readable in plain text.
func renderDelta(yourT, refT []*float64) string {
	out := make([]rune, len(yourT))
	maxAbs := 0.0
	deltas := make([]float64, len(yourT))
	have := make([]bool, len(yourT))
	for i := range yourT {
		if yourT[i] == nil || refT[i] == nil {
			continue
		}
		d := *yourT[i] - *refT[i]
		deltas[i] = d
		have[i] = true
		if math.Abs(d) > maxAbs {
			maxAbs = math.Abs(d)
		}
	}
	if maxAbs == 0 {
		return strings.Repeat(" ", len(yourT))
	}
	for i, d := range deltas {
		if !have[i] {
			out[i] = ' '
			continue
		}
		x := math.Abs(d) / maxAbs
		idx := int(x * float64(len(analogGlyphs)-1))
		if d < 0 {
			// Faster — use lower-case Latin letters as "ahead" markers (a small)
			out[i] = []rune("─━─━─━─━")[idx]
		} else {
			out[i] = analogGlyphs[idx]
		}
	}
	return string(out)
}

// renderCornerRuler places corner ids underneath the strips at the right
// bucket index. Overlapping labels are skipped (first-wins).
func renderCornerRuler(corners []trackmap.Corner, lapLen float64, width int) string {
	sort.Slice(corners, func(i, j int) bool {
		return corners[i].LapDistanceM < corners[j].LapDistanceM
	})
	row := []byte(strings.Repeat(" ", width))
	for _, ch := range corners {
		idx := int(float64(ch.LapDistanceM) / lapLen * float64(width))
		if idx < 0 || idx >= width {
			continue
		}
		label := strings.ToUpper(strings.TrimSpace(ch.ID))
		if label == "" {
			label = ch.Name
		}
		if label == "" {
			continue
		}
		for k := 0; k < len(label) && idx+k < width; k++ {
			if row[idx+k] != ' ' {
				break
			}
			row[idx+k] = label[k]
		}
	}
	return "         " + string(row)
}

func msToLap(ms int) string {
	if ms <= 0 {
		return "(--:--.---)"
	}
	mins := ms / 60000
	secs := (ms % 60000) / 1000
	rem := ms % 1000
	return fmt.Sprintf("(%d:%02d.%03d)", mins, secs, rem)
}

func msToDelta(ms int) string {
	if ms == 0 {
		return "0.000s"
	}
	abs := ms
	if abs < 0 {
		abs = -abs
	}
	secs := abs / 1000
	rem := abs % 1000
	return fmt.Sprintf("%d.%03ds", secs, rem)
}

