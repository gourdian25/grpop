// File: logger_test.go

package grpop

import "testing"

func TestNopLogger_DiscardsEverything(t *testing.T) {
	l := NopLogger()
	// These must not panic; there is nothing else to assert against a
	// deliberately-discarding implementation.
	l.Debug("debug", "k", "v")
	l.Info("info", "k", "v")
	l.Warn("warn", "k", "v")
	l.Error("error", "k", "v")
}

func TestOrNop_NilReturnsNopLogger(t *testing.T) {
	if got := OrNop(nil); got == nil {
		t.Fatal("OrNop(nil) = nil, want a non-nil no-op Logger")
	}
}

func TestOrNop_NonNilPassesThrough(t *testing.T) {
	l := &recordingLogger{}
	if got := OrNop(l); got != l {
		t.Fatalf("OrNop(non-nil) = %v, want the same instance passed in", got)
	}
}
