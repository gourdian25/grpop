// File: retrystrategy_test.go

package grpop

import (
	"testing"
	"time"
)

func TestFullJitterBackoff_Bounds(t *testing.T) {
	base := 100 * time.Millisecond
	max := time.Second
	for attempt := 0; attempt < 10; attempt++ {
		for i := 0; i < 20; i++ {
			d := FullJitterBackoff(base, max, attempt)
			if d < 0 || d > max {
				t.Fatalf("FullJitterBackoff(attempt=%d) = %v, want in [0, %v]", attempt, d, max)
			}
		}
	}
}

func TestFullJitterBackoff_ZeroBase(t *testing.T) {
	if d := FullJitterBackoff(0, time.Second, 0); d != 0 {
		t.Fatalf("FullJitterBackoff(base=0) = %v, want 0", d)
	}
}

func TestFullJitterBackoff_DefaultCeiling(t *testing.T) {
	d := FullJitterBackoff(time.Hour, 0, 5) // max<=0 should fall back to defaultMaxBackoff
	if d > defaultMaxBackoff {
		t.Fatalf("FullJitterBackoff(max<=0) = %v, want <= defaultMaxBackoff (%v)", d, defaultMaxBackoff)
	}
}

func TestFullJitterBackoff_NegativeAttempt(t *testing.T) {
	d := FullJitterBackoff(time.Second, time.Minute, -1)
	if d < 0 || d > time.Minute {
		t.Fatalf("FullJitterBackoff(attempt=-1) = %v, want in [0, time.Minute]", d)
	}
}

func TestFullJitterBackoff_LargeAttemptDoesNotOverflow(t *testing.T) {
	d := FullJitterBackoff(time.Second, time.Minute, 1000)
	if d < 0 || d > time.Minute {
		t.Fatalf("FullJitterBackoff(attempt=1000) = %v, want in [0, time.Minute]", d)
	}
}
