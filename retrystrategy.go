// File: retrystrategy.go

package grpop

import (
	"math/rand"
	"time"
)

// defaultMaxBackoff is the safety ceiling applied by FullJitterBackoff
// whenever a caller passes max<=0.
const defaultMaxBackoff = 30 * time.Second

// FullJitterBackoff returns a randomized backoff duration for the given
// 0-indexed attempt that just failed: sleep = random(0, min(cap,
// base*2^attempt)) — the AWS "Full Jitter" formula. This mirrors
// grevents' retry.go computeBackoff and grnoti's own FullJitterBackoff
// exactly, deliberately kept identical rather than inventing a third copy
// of the same formula. It is exported so it can be shared by both
// Service's inline per-call retry (docs.go, "no message broker") and the
// Postgres/Mongo DLQHandler backends' own NextRetryAt computation
// (Stage 6/7), instead of two independently-written copies.
//
// Parameters:
//   - base: time.Duration — the starting point; base<=0 returns 0
//     (no backoff)
//   - max: time.Duration — the ceiling; max<=0 defaults to
//     defaultMaxBackoff
//   - attempt: int — 0-indexed attempt number
//
// Returns:
//   - time.Duration: a value in [0, min(max, base*2^attempt)]
func FullJitterBackoff(base, max time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	ceiling := max
	if ceiling <= 0 {
		ceiling = defaultMaxBackoff
	}

	exp := ceiling
	if attempt >= 0 && attempt < 62 { // 1<<62 already exceeds any sane base*factor; avoid signed-shift overflow beyond this
		if scaled := base * (1 << uint(attempt)); scaled > 0 && scaled < ceiling { //nolint:gosec // attempt is bounded to [0,62) on this branch
			exp = scaled
		}
	}

	return time.Duration(rand.Int63n(int64(exp) + 1)) //nolint:gosec // backoff jitter has no cryptographic requirement
}
