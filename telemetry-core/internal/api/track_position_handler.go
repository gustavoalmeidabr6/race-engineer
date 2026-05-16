package api

import (
	"fmt"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/state"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/trackmap"
)

// track_position_handler.go answers "where am I, where are the others, and
// what corner is coming up?" for the Live agent.
//
// The endpoint is /api/state/track_position. It is a GET that returns:
//   - track geometry (corners + sector boundaries)
//   - the player's current lap_distance + next corner ahead
//   - all 22 cars' lap_distance + signed gap to player on track
//
// Track geometry comes from the hand-curated workspace/tracks/<id>.json
// files via trackmap.Registry. Per-car position comes from the in-memory
// RaceState.GridPositions snapshot (filled by handleLapData/handleMotion).
//
// Gracefully degrades: tracks without corner data return an empty corners
// array and the agent falls back to raw lap_distance reminders.

// trackCornerView mirrors trackmap.Corner with a stable JSON shape.
type trackCornerView struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	LapDistanceM float32 `json:"lap_distance_m"`
	Type         string  `json:"type"`
}

type trackGeometryView struct {
	Name          string            `json:"name"`
	TrackID       int8              `json:"track_id"`
	LengthM       float32           `json:"length_m"`
	SectorStartsM []float32         `json:"sector_starts_m"` // [0, S2_start, S3_start]
	Corners       []trackCornerView `json:"corners"`         // [] if track has no curated geometry
	Note          string            `json:"_note,omitempty"`
}

// playerPositionView is the player-centric "where am I" block.
type playerPositionView struct {
	CarIndex     uint8        `json:"car_index"`
	DriverName   string       `json:"driver_name,omitempty"`
	Position     uint8        `json:"position"`
	Lap          uint8        `json:"lap"`
	Sector       uint8        `json:"sector"`
	LapDistanceM float32      `json:"lap_distance_m"`
	SpeedKmh     uint16       `json:"speed_kmh"`
	World        worldXYZView `json:"world"`
	// Yaw is the player's heading in radians. F1 25 convention: 0 points
	// along world +z, CCW positive. The map view rotates by -yaw so the
	// player's forward direction points up on screen.
	Yaw          float32      `json:"yaw"`
	NextCorner   *trackCornerWithDistance `json:"next_corner,omitempty"`
}

type trackCornerWithDistance struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	LapDistanceM    float32 `json:"lap_distance_m"`
	Type            string  `json:"type"`
	DistanceAheadM  float32 `json:"distance_ahead_m"` // metres from player to corner, lap-wrapped
}

type worldXYZView struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
	Z float32 `json:"z"`
}

// gridCarView is one entry in the grid array.
type gridCarView struct {
	CarIndex     uint8        `json:"car_index"`
	DriverName   string       `json:"driver_name,omitempty"`
	Position     uint8        `json:"position"`
	LapDistanceM float32      `json:"lap_distance_m"`
	CurrentLap   uint8        `json:"current_lap"`
	Sector       uint8        `json:"sector"`
	PitStatus    string       `json:"pit_status"`
	DriverStatus string       `json:"driver_status"`
	World        worldXYZView `json:"world"`
	// AheadOfMeM is signed distance along the racing line from me to this
	// car. Positive means they are ahead this lap; negative means behind.
	// Computed by lap-wrapped delta of lap_distances; ignores lap-count
	// differences (we are not modelling laps-down here — that comes from
	// car_position which is already on the entry).
	AheadOfMeM float32 `json:"ahead_of_me_m"`
	// Compound + age + pit-stop count come from the in-memory cache so the
	// live map / leaderboard get them every poll without touching DuckDB.
	ActualCompound uint8 `json:"actual_tyre_compound"`
	VisualCompound uint8 `json:"visual_tyre_compound"`
	TyresAgeLaps   uint8 `json:"tyres_age_laps"`
	NumPitStops    uint8 `json:"num_pit_stops"`
}

type trackPositionResponse struct {
	Headline string             `json:"headline"`
	Track    trackGeometryView  `json:"track"`
	Me       playerPositionView `json:"me"`
	Grid     []gridCarView      `json:"grid"`
}

// TrackPositionDynamic is the tick-rate slice of track_position: everything
// that changes per UDP packet. Stable geometry (corners, length, sector
// boundaries) lives in trackGeometryView and is pushed separately — once
// per session via REST, not on every WS tick.
type TrackPositionDynamic struct {
	Headline string             `json:"headline"`
	Me       playerPositionView `json:"me"`
	Grid     []gridCarView      `json:"grid"`
}

// buildTrackGeometry derives the static track block from a snapshot. Output
// shape matches the original trackPositionHandler exactly so REST consumers
// (Live agent, pi agent, curl) see no change.
func buildTrackGeometry(s *models.RaceState, track *trackmap.TrackInfo) trackGeometryView {
	g := trackGeometryView{
		TrackID:       s.TrackID,
		LengthM:       float32(s.TrackLength),
		SectorStartsM: []float32{0, s.Sector2DistStart, s.Sector3DistStart},
		Corners:       []trackCornerView{},
	}
	if track != nil {
		g.Name = track.Name
		g.Note = track.Note
		g.Corners = make([]trackCornerView, 0, len(track.Corners))
		for _, c := range track.Corners {
			g.Corners = append(g.Corners, trackCornerView{
				ID:           c.ID,
				Name:         c.Name,
				LapDistanceM: c.LapDistanceM,
				Type:         string(c.Type),
			})
		}
	}
	return g
}

// BuildTrackPositionDynamic computes the per-tick player + grid + headline
// block. Exported so the WebSocket hub can call it on every broadcast tick
// without re-running the static geometry build.
func BuildTrackPositionDynamic(s *models.RaceState, track *trackmap.TrackInfo, roster *state.Roster) TrackPositionDynamic {
	me := s.GridPositions[s.PlayerCarIndex]
	mePos := playerPositionView{
		CarIndex:     s.PlayerCarIndex,
		Position:     me.Position,
		Lap:          me.CurrentLap,
		Sector:       me.Sector,
		LapDistanceM: s.LapDistance,
		SpeedKmh:     s.Speed,
		World: worldXYZView{
			X: s.WorldPosX,
			Y: s.WorldPosY,
			Z: s.WorldPosZ,
		},
		Yaw: s.Yaw,
	}
	if roster != nil {
		mePos.DriverName = roster.ResolveRadioName(s.PlayerCarIndex)
	}
	if track != nil {
		if next := track.NextCornerAfter(s.LapDistance); next != nil {
			mePos.NextCorner = &trackCornerWithDistance{
				ID:             next.ID,
				Name:           next.Name,
				LapDistanceM:   next.LapDistanceM,
				Type:           string(next.Type),
				DistanceAheadM: lapWrapDelta(s.LapDistance, next.LapDistanceM, float32(s.TrackLength)),
			}
		}
	}

	grid := make([]gridCarView, 0, 22)
	for i := 0; i < 22; i++ {
		gp := s.GridPositions[i]
		if gp.ResultStatus < 2 {
			continue
		}
		gv := gridCarView{
			CarIndex:     gp.CarIndex,
			Position:     gp.Position,
			LapDistanceM: gp.LapDistance,
			CurrentLap:   gp.CurrentLap,
			Sector:       gp.Sector,
			PitStatus:    pitStatusName(gp.PitStatus),
			DriverStatus: driverStatusName(gp.DriverStatus),
			World: worldXYZView{
				X: gp.WorldPosX,
				Y: gp.WorldPosY,
				Z: gp.WorldPosZ,
			},
			ActualCompound: gp.ActualCompound,
			VisualCompound: gp.VisualCompound,
			TyresAgeLaps:   gp.TyresAgeLaps,
			NumPitStops:    gp.NumPitStops,
		}
		if roster != nil {
			gv.DriverName = roster.ResolveRadioName(gp.CarIndex)
		}
		if i != int(s.PlayerCarIndex) {
			gv.AheadOfMeM = signedTrackDelta(s.LapDistance, gp.LapDistance, float32(s.TrackLength))
		}
		grid = append(grid, gv)
	}
	sortGridByPosition(grid)

	headline := fmt.Sprintf("P%d on lap %d, sector %d, %.0fm into lap",
		mePos.Position, mePos.Lap, mePos.Sector+1, mePos.LapDistanceM)
	if mePos.NextCorner != nil {
		headline += fmt.Sprintf(" — %s in %.0fm",
			cornerLabel(*mePos.NextCorner), mePos.NextCorner.DistanceAheadM)
	}

	return TrackPositionDynamic{
		Headline: headline,
		Me:       mePos,
		Grid:     grid,
	}
}

// trackPositionHandler powers GET /api/state/track_position.
func trackPositionHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		s := deps.Store.Cache().Load()
		if s == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "no telemetry yet",
			})
		}

		var track *trackmap.TrackInfo
		if deps.TrackMap != nil {
			track = deps.TrackMap.Lookup(s.TrackID)
		}

		geometry := buildTrackGeometry(s, track)
		dyn := BuildTrackPositionDynamic(s, track, deps.Store.Roster())

		return c.JSON(trackPositionResponse{
			Headline: dyn.Headline,
			Track:    geometry,
			Me:       dyn.Me,
			Grid:     dyn.Grid,
		})
	}
}

// lapWrapDelta returns metres from `from` forward to `to`, wrapping through
// the lap. Always non-negative. Used to compute "metres ahead to next corner".
func lapWrapDelta(from, to, lapLen float32) float32 {
	if lapLen <= 0 {
		return 0
	}
	d := to - from
	for d < 0 {
		d += lapLen
	}
	for d >= lapLen {
		d -= lapLen
	}
	return d
}

// signedTrackDelta returns the signed gap from `me` to `other` along the
// racing line. Positive = other is AHEAD this lap; negative = other is
// BEHIND. Picks the shorter direction so the magnitude is always
// ≤ lapLen / 2.
func signedTrackDelta(me, other, lapLen float32) float32 {
	if lapLen <= 0 {
		return other - me
	}
	d := other - me
	half := lapLen / 2
	for d > half {
		d -= lapLen
	}
	for d < -half {
		d += lapLen
	}
	return d
}

// pitStatusName / driverStatusName: tiny local decoders. We could route via
// the enums package but these are 4-line maps, not worth the import here
// unless other handlers need them.
func pitStatusName(v uint8) string {
	switch v {
	case 0:
		return "on track"
	case 1:
		return "pit lane"
	case 2:
		return "in pit"
	default:
		return "unknown"
	}
}

func driverStatusName(v uint8) string {
	switch v {
	case 0:
		return "in garage"
	case 1:
		return "flying lap"
	case 2:
		return "in lap"
	case 3:
		return "out lap"
	case 4:
		return "on track"
	default:
		return "unknown"
	}
}

func cornerLabel(c trackCornerWithDistance) string {
	if c.Name != "" && c.Name != c.ID {
		return c.ID + " " + c.Name
	}
	return c.ID
}

// sortGridByPosition sorts in place. Entries with position 0 (no data yet)
// sink to the end so the active grid is always at the top.
func sortGridByPosition(g []gridCarView) {
	// Small slice, simple insertion sort keeps allocations zero.
	for i := 1; i < len(g); i++ {
		j := i
		for j > 0 && lessByPos(g[j], g[j-1]) {
			g[j], g[j-1] = g[j-1], g[j]
			j--
		}
	}
}

// lessByPos sorts by car_position ascending, with 0 (no data) sinking last.
// Caller relies on this stable, position-major ordering for the grid array.
func lessByPos(a, b gridCarView) bool {
	// Treat 0 as "missing" — push to the back.
	pa, pb := a.Position, b.Position
	if pa == 0 && pb == 0 {
		return false
	}
	if pa == 0 {
		return false
	}
	if pb == 0 {
		return true
	}
	return pa < pb
}

// Suppress "imported and not used" if we later trim — anchor used to keep
// models import in the file even if all references move. Not active.
var _ = models.RaceState{}
