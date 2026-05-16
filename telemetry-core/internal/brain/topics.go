package brain

// Topic is a typed key for observations and hypotheses. Keeping the registry
// closed (require a code change to add a topic) is the discipline that keeps
// the brain from devolving into a freeform JSON dump.
type Topic string

const (
	TopicTireDegradation   Topic = "tires.degradation"
	TopicTireCliff         Topic = "tires.cliff"
	TopicTireTemp          Topic = "tires.temp"
	TopicCompetitorThreat  Topic = "competitors.threat"
	TopicUndercut          Topic = "competitors.undercut"
	TopicWeatherForecast   Topic = "weather.forecast"
	TopicPitWindow         Topic = "pit.window"
	TopicPitRecommendation Topic = "pit.recommendation"
	TopicFuelTarget        Topic = "fuel.target"
	TopicDamageStatus      Topic = "damage.status"
	TopicPaceTrend         Topic = "pace.lap_trend"
	TopicStrategyMode      Topic = "strategy.mode"
	TopicDriverComplaint   Topic = "driver.complaint"
	TopicDelivery          Topic = "delivery" // event-bus dropped/retried events
)

// AllTopics enumerates known topics in stable order. Snapshot serialization
// iterates this slice so prompts have a deterministic layout (helps LLM
// prompt caching and human diffability of logs).
var AllTopics = []Topic{
	TopicStrategyMode,
	TopicPitWindow,
	TopicPitRecommendation,
	TopicTireDegradation,
	TopicTireCliff,
	TopicTireTemp,
	TopicFuelTarget,
	TopicDamageStatus,
	TopicCompetitorThreat,
	TopicUndercut,
	TopicWeatherForecast,
	TopicPaceTrend,
	TopicDriverComplaint,
	TopicDelivery,
}

// Known reports whether t is in the registry.
func Known(t Topic) bool {
	for _, k := range AllTopics {
		if k == t {
			return true
		}
	}
	return false
}
