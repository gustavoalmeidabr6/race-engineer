//go:build !darwin && !windows && !linux

package audio

import "github.com/rs/zerolog/log"

type noopDucker struct{}

// NewOSDucker is the freebsd/openbsd/etc. fallback. We don't know the
// platform's mixer surface so ducking is a documented no-op rather than
// a build error. mode is accepted for signature compatibility.
func NewOSDucker(targets []string, level float64, mode Mode) (Ducker, error) {
	_ = mode
	log.Info().Msg("audio: ducker noop (unsupported platform); music will not be ducked")
	return noopDucker{}, nil
}

func (noopDucker) SetDucked(bool) error { return nil }
func (noopDucker) Close() error         { return nil }
