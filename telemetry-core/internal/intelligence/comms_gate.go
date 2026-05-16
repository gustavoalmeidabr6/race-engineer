package intelligence

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/transcript"
)

// GateResponse is the structured JSON the LLM returns for each evaluation.
type GateResponse struct {
	CommunicateToDriver bool   `json:"communicate_to_driver"`
	VoiceText           string `json:"voice_text"`
	AnalystInputNeeded  bool   `json:"analyst_input_needed"`
	AnalystPrompt       string `json:"analyst_prompt"`
}

// QueueItem wraps an insight with age tracking.
type QueueItem struct {
	Insight models.DrivingInsight
	Age     int // batch cycles survived without being communicated
}

// driverQuery is an internal request from HTTP handlers.
type driverQuery struct {
	text   string
	result chan<- models.DrivingInsight
}

// CommsGate stages insights, evaluates them via an LLM provider with structured
// JSON output, and decides whether/when to communicate to the driver. All
// telemetry, observations, static context, and recent radio history are pulled
// from the shared RaceBrain at decision time — the gate carries no state of
// its own beyond the pending-insight queue.
type CommsGate struct {
	provider       LLMProvider
	brain          *brain.RaceBrain
	verbosityFn    func() int32
	rawChan        <-chan models.DrivingInsight
	translatedChan chan<- models.DrivingInsight
	driverQueryCh  chan driverQuery
	normalQueue    []QueueItem

	// batchInterval controls how often the normal queue is evaluated.
	// Test code can shrink this to keep tests fast.
	batchInterval time.Duration

	// silentRulePath, when true, makes the gate consume rawChan and batch-tick
	// signals without acting on them — the LLM gate becomes a passive
	// query-only handler. Used when Gemini Live owns the proactive surface
	// via the Interrupts Bus, so rule-engine DrivingInsights don't get
	// re-broadcast through both pipelines. HandleQuery (driver text query)
	// is unaffected and keeps working in this mode.
	silentRulePath bool

	// transcript, when set, receives a copy of every emitted radio line
	// (engineer_speech) and every driver query (user_utterance). nil-safe.
	transcript *transcript.Hub
}

// SetTranscript wires the per-session transcript hub. Optional — when
// nil, all radio output and driver queries are simply not logged to disk.
func (g *CommsGate) SetTranscript(h *transcript.Hub) { g.transcript = h }

// NewCommsGate creates a CommsGate wired to an LLM provider, brain, and channels.
// The brain owns soul/user/driver/track/learnings — the gate no longer reads
// workspace files directly.
func NewCommsGate(
	provider LLMProvider,
	b *brain.RaceBrain,
	verbosityFn func() int32,
	rawChan <-chan models.DrivingInsight,
	translatedChan chan<- models.DrivingInsight,
) *CommsGate {
	return &CommsGate{
		provider:       provider,
		brain:          b,
		verbosityFn:    verbosityFn,
		rawChan:        rawChan,
		translatedChan: translatedChan,
		driverQueryCh:  make(chan driverQuery, 10),
		batchInterval:  15 * time.Second,
	}
}

// SetBatchInterval overrides the normal-batch tick interval. Intended for tests.
func (g *CommsGate) SetBatchInterval(d time.Duration) {
	if d > 0 {
		g.batchInterval = d
	}
}

// SetSilentRulePath enables passive mode: rawChan signals and batch-tick
// processing are dropped on the floor, but driver queries still work. Used
// when Gemini Live owns the proactive radio surface via the Interrupts Bus.
func (g *CommsGate) SetSilentRulePath(silent bool) { g.silentRulePath = silent }

// Run starts the CommsGate loop. Blocks until ctx is cancelled.
func (g *CommsGate) Run(ctx context.Context) {
	log.Info().Msg("CommsGate started")

	batchTimer := time.NewTimer(g.batchInterval)
	defer batchTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("CommsGate stopping")
			return

		case insight, ok := <-g.rawChan:
			if !ok {
				return
			}
			if g.silentRulePath {
				// Drop rule-engine signals — the Interrupts Bus owns the
				// proactive surface in Gemini Live mode.
				continue
			}
			if insight.Priority >= 4 {
				g.processUrgent(ctx, insight)
			} else {
				g.normalQueue = append(g.normalQueue, QueueItem{Insight: insight, Age: 0})
				log.Debug().Int("queue_size", len(g.normalQueue)).Msg("CommsGate: queued normal insight")
			}

		case dq := <-g.driverQueryCh:
			g.handleDriverQuery(ctx, dq)

		case <-batchTimer.C:
			if !g.silentRulePath {
				g.processNormalBatch(ctx)
			}
			batchTimer.Reset(g.batchInterval)
		}
	}
}

// HandleQuery is the public API for HTTP handlers. Sends a driver query through
// the CommsGate pipeline and blocks until a response is ready.
func (g *CommsGate) HandleQuery(ctx context.Context, text string) models.DrivingInsight {
	resultCh := make(chan models.DrivingInsight, 1)
	dq := driverQuery{text: text, result: resultCh}

	select {
	case g.driverQueryCh <- dq:
	case <-ctx.Done():
		return models.DrivingInsight{
			Message:  "Radio timeout. Stand by.",
			Type:     "warning",
			Priority: 3,
		}
	}

	select {
	case result := <-resultCh:
		return result
	case <-time.After(30 * time.Second):
		return models.DrivingInsight{
			Message:  "I'm having trouble with the data link. Stand by.",
			Type:     "warning",
			Priority: 3,
		}
	}
}

// processUrgent evaluates a high-priority insight immediately.
func (g *CommsGate) processUrgent(ctx context.Context, insight models.DrivingInsight) {
	log.Info().Str("msg", insight.Message).Int("priority", insight.Priority).Msg("CommsGate: urgent insight")

	// Pre-translated insights (e.g. analyst-sourced) are already clean radio
	// sentences. Re-translating them just produces a slightly-different
	// phrasing of the same fact — emit raw and skip the LLM call.
	if insight.PreTranslated {
		log.Info().Str("radio", insight.Message).Msg("CommsGate: urgent → driver (pre-translated, raw passthrough)")
		g.emit(insight)
		return
	}

	if !g.provider.Available() {
		g.emit(models.DrivingInsight{
			Message:  "Team: " + insight.Message,
			Type:     "strategy",
			Priority: 4,
		})
		return
	}

	prompt := fmt.Sprintf(
		"URGENT INSIGHT (Priority %d):\n- %s (type: %s)\n\nThis is a critical, time-sensitive message. Evaluate and respond.",
		insight.Priority, insight.Message, insight.Type,
	)

	system, pcfg := g.gateConfig(true)

	resp, err := GenerateJSON[GateResponse](g.provider, ctx, prompt, system, pcfg)
	if err != nil {
		log.Warn().Err(err).Msg("CommsGate: urgent LLM call failed, passing through")
		g.emit(models.DrivingInsight{
			Message:  "Team: " + insight.Message,
			Type:     "strategy",
			Priority: 4,
		})
		return
	}

	if resp.CommunicateToDriver && resp.VoiceText != "" {
		log.Info().Str("radio", resp.VoiceText).Msg("CommsGate: urgent → driver")
		g.emit(models.DrivingInsight{
			Message:  strings.TrimSpace(resp.VoiceText),
			Type:     "strategy",
			Priority: 4,
		})
	} else {
		// Rare for urgent, but move to normal queue for re-eval.
		g.normalQueue = append(g.normalQueue, QueueItem{Insight: insight, Age: 0})
		log.Info().Msg("CommsGate: urgent held for re-evaluation")
	}

	if resp.AnalystInputNeeded {
		log.Info().Str("prompt", resp.AnalystPrompt).Msg("CommsGate: analyst input requested")
	}
}

// processNormalBatch evaluates all queued normal insights.
// Strategy analyst insights (type=strategy) bypass the LLM gate after 1 cycle —
// they were already vetted by the analyst. Rule-engine insights go through the gate.
func (g *CommsGate) processNormalBatch(ctx context.Context) {
	if len(g.normalQueue) == 0 {
		return
	}

	// Increment age and evict stale items (>= 3 cycles).
	var kept []QueueItem
	for _, item := range g.normalQueue {
		item.Age++
		if item.Age >= 3 {
			log.Warn().Str("msg", item.Insight.Message).Int("age", item.Age).Msg("CommsGate: evicting stale insight")
			continue
		}
		kept = append(kept, item)
	}
	g.normalQueue = kept

	if len(g.normalQueue) == 0 {
		return
	}

	// Split: strategy analyst insights get translated directly (already vetted).
	// Rule-engine insights go through the LLM gate.
	var strategyItems []QueueItem
	var ruleItems []QueueItem
	for _, item := range g.normalQueue {
		if item.Insight.Type == "strategy" && item.Age >= 1 {
			strategyItems = append(strategyItems, item)
		} else {
			ruleItems = append(ruleItems, item)
		}
	}

	for _, item := range strategyItems {
		g.translateAndEmit(ctx, item.Insight)
	}

	g.normalQueue = ruleItems

	if len(ruleItems) == 0 {
		return
	}

	log.Debug().Int("items", len(ruleItems)).Msg("CommsGate: evaluating rule-engine batch")

	if !g.provider.Available() {
		var messages []string
		for _, item := range ruleItems {
			messages = append(messages, item.Insight.Message)
		}
		g.emit(models.DrivingInsight{
			Message:  "Team: " + strings.Join(messages, "; "),
			Type:     "strategy",
			Priority: 3,
		})
		g.normalQueue = nil
		return
	}

	var insightLines []string
	for _, item := range ruleItems {
		insightLines = append(insightLines, fmt.Sprintf("- %s (type: %s, priority: %d, age: %d cycles)",
			item.Insight.Message, item.Insight.Type, item.Insight.Priority, item.Age))
	}

	prompt := fmt.Sprintf(
		"PENDING INSIGHTS (%d items):\n%s\n\nEvaluate whether these insights, taken together, warrant a radio message to the driver right now.",
		len(ruleItems), strings.Join(insightLines, "\n"),
	)

	system, pcfg := g.gateConfig(false)

	resp, err := GenerateJSON[GateResponse](g.provider, ctx, prompt, system, pcfg)
	if err != nil {
		log.Warn().Err(err).Msg("CommsGate: normal batch LLM call failed")
		return
	}

	if resp.CommunicateToDriver {
		voiceText := strings.TrimSpace(resp.VoiceText)
		if voiceText == "" {
			var msgs []string
			for _, item := range ruleItems {
				msgs = append(msgs, item.Insight.Message)
			}
			voiceText = strings.Join(msgs, ". ")
		}
		log.Info().Str("radio", voiceText).Int("items", len(ruleItems)).Msg("CommsGate: batch → driver")
		g.emit(models.DrivingInsight{
			Message:  voiceText,
			Type:     "strategy",
			Priority: 3,
		})
		g.normalQueue = nil
	} else {
		log.Debug().Int("items", len(ruleItems)).Msg("CommsGate: batch held for next cycle")
	}

	if resp.AnalystInputNeeded {
		log.Info().Str("prompt", resp.AnalystPrompt).Msg("CommsGate: analyst input requested")
	}
}

// handleDriverQuery evaluates a driver question with pending context.
func (g *CommsGate) handleDriverQuery(ctx context.Context, dq driverQuery) {
	log.Info().Str("query", dq.text).Msg("CommsGate: driver query")

	if g.transcript != nil {
		g.transcript.Append(transcript.Event{
			Kind:  transcript.KindUserUtterance,
			Actor: "driver",
			Text:  dq.text,
			Meta:  map[string]any{"source": "comms_gate"},
		})
	}

	snap := g.snapshot()
	if snap.HotFacts == nil {
		result := models.DrivingInsight{
			Message:  "I don't have any telemetry data yet. Stand by.",
			Type:     "info",
			Priority: 3,
		}
		g.emitAndRespond(result, dq)
		return
	}

	if !g.provider.Available() {
		result := models.DrivingInsight{
			Message:  "I don't have an AI connection right now. Stand by.",
			Type:     "info",
			Priority: 3,
		}
		g.emitAndRespond(result, dq)
		return
	}

	var pendingContext string
	if len(g.normalQueue) > 0 {
		var lines []string
		for _, item := range g.normalQueue {
			lines = append(lines, fmt.Sprintf("- %s (type: %s, priority: %d)", item.Insight.Message, item.Insight.Type, item.Insight.Priority))
		}
		pendingContext = fmt.Sprintf("\n\nPENDING UNSENT INSIGHTS:\n%s", strings.Join(lines, "\n"))
	}

	prompt := fmt.Sprintf(
		"DRIVER IS ASKING A QUESTION — you MUST set communicate_to_driver to true.\n\n"+
			"Driver's question: \"%s\"%s\n\n"+
			"Answer the driver's question using the live telemetry context. "+
			"If there are pending insights relevant to the question, incorporate them into your answer.",
		dq.text, pendingContext,
	)

	system, pcfg := g.gateConfigFromSnapshot(snap, true)

	resp, err := GenerateJSON[GateResponse](g.provider, ctx, prompt, system, pcfg)
	if err != nil {
		log.Warn().Err(err).Msg("CommsGate: driver query LLM call failed")
		result := models.DrivingInsight{
			Message:  "I'm having trouble with the data connection.",
			Type:     "warning",
			Priority: 4,
		}
		g.emitAndRespond(result, dq)
		return
	}

	voiceText := resp.VoiceText
	if voiceText == "" {
		voiceText = "Copy, stand by."
	}

	result := models.DrivingInsight{
		Message:  strings.TrimSpace(voiceText),
		Type:     "info",
		Priority: 4,
	}

	g.normalQueue = nil
	g.emitAndRespond(result, dq)

	if resp.AnalystInputNeeded {
		log.Info().Str("prompt", resp.AnalystPrompt).Msg("CommsGate: analyst input requested (driver query)")
	}
}

// emitAndRespond sends the insight to the fanout pipeline and responds to the driver query.
func (g *CommsGate) emitAndRespond(insight models.DrivingInsight, dq driverQuery) {
	g.emit(insight)
	select {
	case dq.result <- insight:
	default:
	}
}

// translateAndEmit translates a strategy analyst insight into radio speech and emits it.
func (g *CommsGate) translateAndEmit(ctx context.Context, insight models.DrivingInsight) {
	// Already-conversational insights (analyst path sets PreTranslated=true)
	// shouldn't be re-translated.
	if insight.PreTranslated {
		log.Info().Str("radio", insight.Message).Msg("CommsGate: strategy insight → driver (pre-translated, raw passthrough)")
		g.emit(insight)
		return
	}

	if !g.provider.Available() {
		g.emit(models.DrivingInsight{
			Message:  insight.Message,
			Type:     "strategy",
			Priority: insight.Priority,
		})
		return
	}

	prompt := fmt.Sprintf(
		"STRATEGY ANALYST INSIGHT (already vetted — translate to radio speech, always communicate):\n%s",
		insight.Message,
	)

	system, pcfg := g.gateConfig(false)

	resp, err := GenerateJSON[GateResponse](g.provider, ctx, prompt, system, pcfg)
	if err != nil || strings.TrimSpace(resp.VoiceText) == "" {
		log.Warn().Err(err).Str("msg", insight.Message).Msg("CommsGate: translation failed, passing through raw")
		g.emit(models.DrivingInsight{
			Message:  insight.Message,
			Type:     "strategy",
			Priority: insight.Priority,
		})
		return
	}

	log.Info().Str("radio", resp.VoiceText).Msg("CommsGate: strategy insight → driver")
	g.emit(models.DrivingInsight{
		Message:  strings.TrimSpace(resp.VoiceText),
		Type:     "strategy",
		Priority: insight.Priority,
	})
}

// emit sends a translated insight to the output channel (non-blocking)
// and mirrors it as an engineer_speech transcript event.
func (g *CommsGate) emit(insight models.DrivingInsight) {
	select {
	case g.translatedChan <- insight:
	default:
		log.Warn().Msg("CommsGate: translated insight channel full, dropping")
	}
	if g.transcript != nil {
		g.transcript.Append(transcript.Event{
			Kind:  transcript.KindEngineerSpeech,
			Actor: "comms_gate",
			Text:  insight.Message,
			Meta: map[string]any{
				"priority": insight.Priority,
				"type":     insight.Type,
			},
		})
	}
}

// snapshot pulls the standard view from the brain. Returns an empty snapshot if
// the brain is nil (only happens in misconfigured tests).
func (g *CommsGate) snapshot() brain.Snapshot {
	if g.brain == nil {
		return brain.Snapshot{Now: time.Now()}
	}
	return g.brain.Snapshot(brain.DefaultSnapshotOpts())
}

// gateConfig builds a system prompt + ProviderConfig from the live brain.
// Equivalent to gateConfigFromSnapshot(g.snapshot(), isCritical) — kept as a
// convenience for hot paths that don't already hold a snapshot.
func (g *CommsGate) gateConfig(isCritical bool) (string, ProviderConfig) {
	return g.gateConfigFromSnapshot(g.snapshot(), isCritical)
}

// gateConfigFromSnapshot builds the system prompt from a snapshot. Splitting
// this out lets driver-query handling reuse a snapshot it already inspected.
func (g *CommsGate) gateConfigFromSnapshot(snap brain.Snapshot, isCritical bool) (string, ProviderConfig) {
	verbosityInstr := g.verbosityInstruction()

	soul := snap.Static.Soul
	if soul == "" {
		soul = "You are an assertive but calm F1 Race Engineer."
	}
	user := snap.Static.User
	if user == "" {
		user = "The driver you are talking to."
	}

	var liveTelemetry string
	if snap.HotFacts != nil {
		liveTelemetry = BuildContext(snap.HotFacts)
	} else {
		liveTelemetry = "No telemetry data available yet."
	}

	dynamic := snap.MarkdownDynamic()
	staticBlock := snap.MarkdownStatic()
	transcriptBlock := g.recentTranscriptMarkdown()

	systemPrompt := fmt.Sprintf(
		"%s\n\nDRIVER PREFERENCES:\n%s\n\n"+
			"You are an F1 Race Engineer evaluating incoming data to decide what to communicate "+
			"to your driver over the radio. You must respond with a JSON object with these fields:\n"+
			"- communicate_to_driver (boolean): true if this should be communicated now\n"+
			"- voice_text (string): the radio message in authentic F1 style, empty if not communicating\n"+
			"- analyst_input_needed (boolean): true if deeper data analysis is needed\n"+
			"- analyst_prompt (string): what analysis is needed, 'NONE' if not needed\n\n"+
			"If you decide to communicate, the voice_text must sound like authentic F1 radio "+
			"(e.g., 'Box box', 'Target lap time X', 'Scenario 7'). "+
			"Do NOT add conversational filler like 'Hey' or 'Alright'. Get straight to the point.\n"+
			"Do NOT start with acknowledgement phrases like 'Copy that', 'Roger', 'Understood', "+
			"'Got it', 'Received', 'Affirm', 'Solid copy' — the radio system already sends an "+
			"automatic acknowledgement before your response. Jump straight into the information.\n"+
			"VERBOSITY INSTRUCTION: %s\n"+
			"CRITICAL MESSAGE: %v\n\n"+
			"## Live Telemetry\n%s\n\n"+
			"%s%s%s",
		soul, user, verbosityInstr, isCritical, liveTelemetry, dynamic, transcriptBlock, staticBlock,
	)

	return systemPrompt, ProviderConfig{
		MaxTokens:   5000,
		Temperature: 0.3,
	}
}

// recentTranscriptMarkdown returns a "## Recent Transcript" block for
// prompt injection, drawn from the in-memory ring of the per-session
// log. Empty string if the hub isn't wired or has nothing to show.
func (g *CommsGate) recentTranscriptMarkdown() string {
	if g.transcript == nil {
		return ""
	}
	events := g.transcript.Recent(0, nil)
	if len(events) == 0 {
		return ""
	}
	return "## Recent Transcript\n" + transcript.RenderMarkdown(events) + "\n\n"
}

// verbosityInstruction returns a prompt instruction based on the current verbosity level.
func (g *CommsGate) verbosityInstruction() string {
	verbosity := int32(5)
	if g.verbosityFn != nil {
		verbosity = g.verbosityFn()
	}
	switch {
	case verbosity <= 2:
		return "Be EXTREMELY brief — 5 words max. Pure commands only: 'Box box', 'Push now', 'Copy'."
	case verbosity <= 4:
		return "Be very concise — one short sentence max. No explanations, just the action."
	case verbosity <= 6:
		return "Be concise but include key context — one or two sentences."
	case verbosity <= 8:
		return "Give a clear message with supporting details — two to three sentences."
	default:
		return "Give a detailed explanation with data and reasoning — up to four sentences."
	}
}
