package transcript

import (
	"bytes"
	"encoding/json"
	"strings"
)

// LogWriter is an io.Writer that consumes zerolog's JSON-encoded event
// stream and emits a transcript Event per log line. Designed to be
// composed with the existing ConsoleWriter via zerolog.MultiLevelWriter
// so the terminal output keeps working unchanged.
//
//	log.Logger = log.Output(zerolog.MultiLevelWriter(
//	    consoleWriter,
//	    transcript.NewLogWriter(hub),
//	))
//
// To prevent feedback loops (the hub itself logs warns; those warns
// would re-enter this writer and could spin), any message whose `text`
// or any meta value starts with "transcript:" is silently dropped.
type LogWriter struct {
	hub *Hub
}

// NewLogWriter binds a writer to the given hub. nil-safe — passing a
// nil hub turns Write into a no-op (the caller can still register the
// writer without conditionals).
func NewLogWriter(h *Hub) *LogWriter { return &LogWriter{hub: h} }

// Write parses one JSON log line and forwards it as a transcript event.
// Returns len(p), nil even on parse failure so it doesn't break the
// upstream zerolog writer chain.
func (w *LogWriter) Write(p []byte) (int, error) {
	if w == nil || w.hub == nil {
		return len(p), nil
	}
	raw := bytes.TrimSpace(p)
	if len(raw) == 0 || raw[0] != '{' {
		return len(p), nil
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		return len(p), nil
	}

	level, _ := rec["level"].(string)
	msg, _ := rec["message"].(string)

	// Drop transcript's own log lines to prevent feedback loops.
	if strings.HasPrefix(msg, "transcript:") {
		return len(p), nil
	}

	kind := logLevelToKind(level)
	if kind == "" {
		return len(p), nil
	}

	// Strip noisy stock fields from meta; keep everything else so domain
	// keys (component, packet_id, pid, …) survive into the debug stream.
	delete(rec, "level")
	delete(rec, "time")
	delete(rec, "message")
	if len(rec) == 0 {
		rec = nil
	}

	// Best-effort actor: prefer a "component" or "module" field; else
	// fall back to a flat "telemetry-core" tag so the row colour-codes
	// distinctly from rule_engine / analyst / gemini_live events.
	actor := "telemetry-core"
	for _, k := range []string{"component", "module", "actor", "src"} {
		if v, ok := rec[k].(string); ok && v != "" {
			actor = v
			break
		}
	}

	w.hub.Append(Event{
		Kind:  kind,
		Actor: actor,
		Text:  msg,
		Meta:  rec,
	})
	return len(p), nil
}

func logLevelToKind(level string) Kind {
	switch strings.ToLower(level) {
	case "trace", "debug":
		return KindLogDebug
	case "info":
		return KindLogInfo
	case "warn", "warning":
		return KindLogWarn
	case "error", "fatal", "panic":
		return KindLogError
	}
	return ""
}
