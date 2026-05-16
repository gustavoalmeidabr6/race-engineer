package intelligence

import (
	"context"
	"errors"
	"sync"
)

// MockProvider is a deterministic LLMProvider for tests. Scripts a queue of
// responses (text or error per call) and records every call so tests can
// assert on prompt content. Safe for concurrent use.
type MockProvider struct {
	mu sync.Mutex

	// available is what Available() returns. Defaults to true.
	available bool
	closed    bool

	// completeQueue is consumed in order by Complete and CompleteWithTools.
	// If empty, the response defaults to ("", nil) to surface unset expectations.
	completeQueue []completeResp
	toolsQueue    []toolsResp

	// calls records every Complete / CompleteWithTools invocation.
	calls []ProviderCall
}

type completeResp struct {
	text string
	err  error
}

type toolsResp struct {
	result *GenerateResult
	err    error
}

// ProviderCall captures one LLM call for later assertion.
type ProviderCall struct {
	System string
	Prompt string
	Cfg    ProviderConfig
	Tools  []ToolDef
}

// NewMockProvider returns an available mock with no scripted responses.
func NewMockProvider() *MockProvider {
	return &MockProvider{available: true}
}

// SetAvailable toggles the Available() return value.
func (m *MockProvider) SetAvailable(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.available = v
}

// QueueText enqueues a successful Complete response.
func (m *MockProvider) QueueText(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completeQueue = append(m.completeQueue, completeResp{text: text})
}

// QueueError enqueues an error response for the next Complete call.
func (m *MockProvider) QueueError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completeQueue = append(m.completeQueue, completeResp{err: err})
}

// QueueToolResult enqueues a CompleteWithTools response.
func (m *MockProvider) QueueToolResult(r *GenerateResult, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolsQueue = append(m.toolsQueue, toolsResp{result: r, err: err})
}

// Calls returns a copy of all recorded calls.
func (m *MockProvider) Calls() []ProviderCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ProviderCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// LastCall returns the most recent call or false if none.
func (m *MockProvider) LastCall() (ProviderCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return ProviderCall{}, false
	}
	return m.calls[len(m.calls)-1], true
}

// Reset clears scripted responses and recorded calls.
func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completeQueue = nil
	m.toolsQueue = nil
	m.calls = nil
}

// --- LLMProvider interface ---

func (m *MockProvider) Available() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.available && !m.closed
}

func (m *MockProvider) Complete(_ context.Context, system, prompt string, cfg ProviderConfig) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, ProviderCall{System: system, Prompt: prompt, Cfg: cfg})
	if len(m.completeQueue) == 0 {
		return "", errors.New("MockProvider: no scripted Complete response")
	}
	resp := m.completeQueue[0]
	m.completeQueue = m.completeQueue[1:]
	return resp.text, resp.err
}

func (m *MockProvider) CompleteWithTools(_ context.Context, system, prompt string, cfg ProviderConfig, tools []ToolDef) (*GenerateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, ProviderCall{System: system, Prompt: prompt, Cfg: cfg, Tools: tools})
	// Prefer a scripted tool response when available.
	if len(m.toolsQueue) > 0 {
		resp := m.toolsQueue[0]
		m.toolsQueue = m.toolsQueue[1:]
		return resp.result, resp.err
	}
	// Fall back to the Complete queue so existing tests that script text /
	// error responses via QueueText / QueueError keep working when the code
	// switches to CompleteWithTools (e.g. AnswerQuestion now offers the
	// interrupt_engineer tool when a bus is wired).
	if len(m.completeQueue) > 0 {
		resp := m.completeQueue[0]
		m.completeQueue = m.completeQueue[1:]
		return &GenerateResult{Text: resp.text}, resp.err
	}
	return &GenerateResult{}, nil
}

func (m *MockProvider) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
}
