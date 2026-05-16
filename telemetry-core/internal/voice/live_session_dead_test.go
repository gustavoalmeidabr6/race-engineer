package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

func TestIsLiveSessionDead(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nil", context.Background(), nil, false},
		{"plain transient", context.Background(), errors.New("model temporarily unavailable"), false},
		{"ws close sent", context.Background(), errors.New("websocket: close sent"), true},
		{"closed network", context.Background(), errors.New("write: use of closed network connection"), true},
		{"net.ErrClosed", context.Background(), fmt.Errorf("send: %w", net.ErrClosed), true},
		{"io.EOF", context.Background(), io.EOF, true},
		{"io.ErrUnexpectedEOF", context.Background(), io.ErrUnexpectedEOF, true},
		{"context.Canceled", context.Background(), context.Canceled, true},
		{"context.DeadlineExceeded", context.Background(), context.DeadlineExceeded, true},
		{"cancelled ctx + transient err", cancelled, errors.New("transient"), true},
		{"broken pipe", context.Background(), errors.New("write tcp: broken pipe"), true},
		{"connection reset", context.Background(), errors.New("read: connection reset by peer"), true},
		{"close 1011", context.Background(), errors.New("websocket: close 1011 (internal server error)"), true},
		{"close 1006", context.Background(), errors.New("websocket: close 1006 (abnormal closure)"), true},
		{"rate limit retryable", context.Background(), errors.New("RESOURCE_EXHAUSTED: quota"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isLiveSessionDead(tc.ctx, tc.err)
			if got != tc.want {
				t.Errorf("isLiveSessionDead(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
