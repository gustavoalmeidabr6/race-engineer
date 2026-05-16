package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// nowFn is overridable in tests for deterministic Observation timestamps.
var nowFn = time.Now

func secsToDuration(s float64) time.Duration {
	if s <= 0 {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

// clampPriority enforces the per-server max-priority cap on push_insight.
// Values <1 normalise to 1; values >cap normalise to cap.
func clampPriority(p, cap int) int {
	if p < 1 {
		return 1
	}
	if cap < 1 {
		cap = 1
	}
	if p > cap {
		return cap
	}
	return p
}

func (s *Server) httpDelete(ctx context.Context, path string) (string, error) {
	if s.deps.APIBase == "" {
		return "", fmt.Errorf("APIBase not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.deps.APIBase+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("upstream %s returned %d: %s", path, resp.StatusCode, truncate(string(body), 400))
	}
	return string(body), nil
}
