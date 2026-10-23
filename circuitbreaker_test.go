// File: circuitbreaker_test.go

package grpop

import (
	"context"
	"errors"
	"net/textproto"
	"sync"
	"testing"
	"time"
)

type recordingLogger struct {
	mu                 sync.Mutex
	infos, warns, errs []string
}

func (l *recordingLogger) Debug(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}
func (l *recordingLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}
func (l *recordingLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *recordingLogger) Error(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, msg)
}
func (l *recordingLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

func TestCircuitBreaker_OpensAfterMaxFailures(t *testing.T) {
	cb, err := NewCircuitBreaker(3, 50*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}

	failing := errors.New("boom")
	for i := 0; i < 3; i++ {
		_ = cb.Execute(context.Background(), func() error { return failing })
	}

	if got := cb.State(); got != CircuitStateOpen {
		t.Fatalf("State() = %s, want %s", got, CircuitStateOpen)
	}

	err = cb.Execute(context.Background(), func() error { return nil })
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Execute while open error = %v, want ErrCircuitOpen", err)
	}
}

func TestCircuitBreaker_HalfOpenThenCloses(t *testing.T) {
	cb, err := NewCircuitBreaker(1, 20*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}

	_ = cb.Execute(context.Background(), func() error { return errors.New("boom") })
	if got := cb.State(); got != CircuitStateOpen {
		t.Fatalf("State() = %s, want %s", got, CircuitStateOpen)
	}

	time.Sleep(30 * time.Millisecond)

	if err := cb.Execute(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("Execute (trial request) = %v, want nil", err)
	}
	if got := cb.State(); got != CircuitStateClosed {
		t.Fatalf("State() after successful trial = %s, want %s", got, CircuitStateClosed)
	}
}

func TestCircuitBreaker_LogsStateTransitions(t *testing.T) {
	logger := &recordingLogger{}
	cb, err := NewCircuitBreakerWithConfig(CircuitBreakerConfig{
		MaxFailures:  1,
		Timeout:      20 * time.Millisecond,
		ResetTimeout: time.Hour,
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("NewCircuitBreakerWithConfig: %v", err)
	}

	_ = cb.Execute(context.Background(), func() error { return errors.New("boom") })
	if got := cb.State(); got != CircuitStateOpen {
		t.Fatalf("State() = %s, want %s", got, CircuitStateOpen)
	}
	if got := logger.warnCount(); got == 0 {
		t.Fatal("expected at least one Warn-level log when the breaker opened, got none")
	}

	time.Sleep(30 * time.Millisecond)

	if err := cb.Execute(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("Execute (trial request) = %v, want nil", err)
	}
	if got := cb.State(); got != CircuitStateClosed {
		t.Fatalf("State() after successful trial = %s, want %s", got, CircuitStateClosed)
	}
	logger.mu.Lock()
	infoCount := len(logger.infos)
	logger.mu.Unlock()
	if infoCount == 0 {
		t.Fatal("expected at least one Info-level log across half-open/close transitions, got none")
	}
}

func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	cb, err := NewCircuitBreaker(1, 20*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	_ = cb.Execute(context.Background(), func() error { return errors.New("boom") })
	time.Sleep(30 * time.Millisecond)

	_ = cb.Execute(context.Background(), func() error { return errors.New("boom again") })
	if got := cb.State(); got != CircuitStateOpen {
		t.Fatalf("State() after half-open failure = %s, want %s", got, CircuitStateOpen)
	}
}

func TestCircuitBreaker_Reset(t *testing.T) {
	cb, _ := NewCircuitBreaker(1, time.Hour, time.Hour)
	_ = cb.Execute(context.Background(), func() error { return errors.New("boom") })
	if got := cb.State(); got != CircuitStateOpen {
		t.Fatalf("State() = %s, want %s", got, CircuitStateOpen)
	}
	cb.Reset()
	if got := cb.State(); got != CircuitStateClosed {
		t.Fatalf("State() after Reset = %s, want %s", got, CircuitStateClosed)
	}
}

func TestCircuitBreaker_InvalidConfig(t *testing.T) {
	if _, err := NewCircuitBreaker(0, time.Second, time.Second); err == nil {
		t.Fatal("NewCircuitBreaker(maxFailures=0) = nil error, want non-nil")
	}
	if _, err := NewCircuitBreaker(1, 0, time.Second); err == nil {
		t.Fatal("NewCircuitBreaker(timeout=0) = nil error, want non-nil")
	}
	if _, err := NewCircuitBreaker(1, time.Second, 0); err == nil {
		t.Fatal("NewCircuitBreaker(resetTimeout=0) = nil error, want non-nil")
	}
}

func TestCircuitBreaker_Execute_CanceledContext(t *testing.T) {
	cb, _ := NewCircuitBreaker(3, time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cb.Execute(ctx, func() error { return nil }); err == nil {
		t.Fatal("Execute(canceled ctx) = nil error, want non-nil")
	}
}

func TestNewCircuitBreakerWithConfig_DefaultsMaxHalfOpenRequests(t *testing.T) {
	cb, err := NewCircuitBreakerWithConfig(CircuitBreakerConfig{
		MaxFailures: 1, Timeout: time.Hour, ResetTimeout: time.Hour, MaxHalfOpenRequests: 0,
	})
	if err != nil {
		t.Fatalf("NewCircuitBreakerWithConfig: %v", err)
	}
	scb := cb.(*standardCircuitBreaker)
	if scb.config.MaxHalfOpenRequests != 1 {
		t.Fatalf("MaxHalfOpenRequests = %d, want 1 (the default)", scb.config.MaxHalfOpenRequests)
	}
}

func TestCircuitBreaker_GetStats_OpenState(t *testing.T) {
	cb, _ := NewCircuitBreaker(1, 100*time.Millisecond, time.Hour)
	_ = cb.Execute(context.Background(), func() error { return errors.New("boom") })

	stats := cb.GetStats()
	if stats.State != CircuitStateOpen {
		t.Fatalf("GetStats().State = %s, want %s", stats.State, CircuitStateOpen)
	}
	if stats.TimeUntilNextAttempt <= 0 {
		t.Fatalf("GetStats().TimeUntilNextAttempt = %v, want > 0 while still within Timeout", stats.TimeUntilNextAttempt)
	}
	if stats.OpenedAt.IsZero() {
		t.Fatal("GetStats().OpenedAt is zero, want set once opened")
	}

	time.Sleep(150 * time.Millisecond)
	stats = cb.GetStats()
	if stats.TimeUntilNextAttempt != 0 {
		t.Fatalf("GetStats().TimeUntilNextAttempt = %v after Timeout elapsed, want 0", stats.TimeUntilNextAttempt)
	}
}

// TestCircuitBreaker_ConcurrentExecute proves the state machine's mutex
// protection under real concurrent load, not just single-threaded review.
func TestCircuitBreaker_ConcurrentExecute(t *testing.T) {
	cb, _ := NewCircuitBreakerWithConfig(CircuitBreakerConfig{
		MaxFailures: 5, Timeout: 10 * time.Millisecond, ResetTimeout: time.Hour, MaxHalfOpenRequests: 2,
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = cb.Execute(context.Background(), func() error {
				if i%2 == 0 {
					return errors.New("boom")
				}
				return nil
			})
			_ = cb.GetStats()
			_ = cb.State()
		}(i)
	}
	wg.Wait()
}

func TestCircuitBreaker_TooManyRequestsWhileHalfOpen(t *testing.T) {
	cb, err := NewCircuitBreakerWithConfig(CircuitBreakerConfig{
		MaxFailures: 1, Timeout: 10 * time.Millisecond, ResetTimeout: time.Hour, MaxHalfOpenRequests: 1,
	})
	if err != nil {
		t.Fatalf("NewCircuitBreakerWithConfig: %v", err)
	}

	// Trip the breaker open.
	_ = cb.Execute(context.Background(), func() error { return errors.New("fail") })
	if cb.State() != CircuitStateOpen {
		t.Fatalf("State() = %v, want CircuitStateOpen", cb.State())
	}
	time.Sleep(15 * time.Millisecond) // let Timeout elapse

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_ = cb.Execute(context.Background(), func() error {
			close(started)
			<-release
			return nil
		})
		close(done)
	}()

	<-started // the first half-open trial is now in flight, holding the single MaxHalfOpenRequests slot
	if err := cb.Execute(context.Background(), func() error { return nil }); !errors.Is(err, ErrTooManyRequests) {
		t.Fatalf("second concurrent Execute() while half-open err = %v, want ErrTooManyRequests", err)
	}

	close(release)
	<-done
}

// TestExecuteThroughBreaker: a permanent failure comes back unchanged and
// doesn't count against the vendor (the breaker records the answer as a
// success); a temporary one counts, and enough of them open the breaker.
func TestExecuteThroughBreaker(t *testing.T) {
	refused := permanent(errors.New("mailbox unknown"))
	if err := executeThroughBreaker(context.Background(), nil, func() error { return refused }); err != refused {
		t.Fatalf("with no breaker = %v, want the send's own error", err)
	}

	cb, err := NewCircuitBreaker(2, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	calls := 0
	for i := 0; i < 10; i++ {
		if err := executeThroughBreaker(context.Background(), cb, func() error { calls++; return refused }); err != refused {
			t.Fatalf("permanent failure %d = %v, want it unchanged", i, err)
		}
	}
	if stats := cb.GetStats(); stats.State != CircuitStateClosed || stats.TotalFailures != 0 || stats.ConsecutiveFailures != 0 {
		t.Fatalf("after ten permanent failures: %+v, want closed with no failures counted", stats)
	}

	down := errors.New("connection refused")
	for i := 0; i < 2; i++ {
		if err := executeThroughBreaker(context.Background(), cb, func() error { calls++; return down }); err != down {
			t.Fatalf("temporary failure %d = %v, want it unchanged", i, err)
		}
	}
	if cb.State() != CircuitStateOpen {
		t.Fatalf("after two temporary failures the breaker is %s, want open", cb.State())
	}
	if err := executeThroughBreaker(context.Background(), cb, func() error { calls++; return nil }); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("with the breaker open = %v, want ErrCircuitOpen", err)
	}
	if calls != 12 {
		t.Fatalf("send ran %d times, want 12 (not while open)", calls)
	}
}

// TestSenders_BreakerCountsOnlyTemporaryFailures: each sender's vendor
// refusals (SMTP 550, Resend 422, a Meta refusal) leave a one-failure
// breaker closed, so the next message still goes; a temporary failure
// opens it.
func TestSenders_BreakerCountsOnlyTemporaryFailures(t *testing.T) {
	ctx := context.Background()
	email := EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"}
	cases := []struct {
		name string
		// build returns a send the vendor refuses, a send that fails
		// temporarily, and how many calls reached the vendor, all through cb.
		build func(t *testing.T, cb CircuitBreaker) (refused, temporary func() error, vendorCalls func() int)
	}{
		{"smtp", func(t *testing.T, cb CircuitBreaker) (func() error, func() error, func() int) {
			refusing, refusingDialer := newTestSMTPDispatcher(t, &fakeSMTPClient{rcptErr: &textproto.Error{Code: 550, Msg: "no such user"}},
				func(d *SMTPDispatcherDeps) { d.CircuitBreaker = cb })
			failing, failingDialer := newTestSMTPDispatcher(t, &fakeSMTPClient{rcptErr: &textproto.Error{Code: 451, Msg: "try later"}},
				func(d *SMTPDispatcherDeps) { d.CircuitBreaker = cb })
			return func() error { _, err := refusing.Send(ctx, email); return err },
				func() error { _, err := failing.Send(ctx, email); return err },
				func() int { return refusingDialer.calls() + failingDialer.calls() }
		}},
		{"resend", func(t *testing.T, cb CircuitBreaker) (func() error, func() error, func() int) {
			refusingAPI := &fakeResend{status: 422, body: `{"name":"validation_error","message":"Invalid to"}`}
			failingAPI := &fakeResend{status: 500, body: `{"name":"application_error","message":"Oops"}`}
			refusing := newTestResendDispatcher(t, refusingAPI, func(d *ResendDispatcherDeps) { d.CircuitBreaker = cb })
			failing := newTestResendDispatcher(t, failingAPI, func(d *ResendDispatcherDeps) { d.CircuitBreaker = cb })
			return func() error { _, err := refusing.Send(ctx, email); return err },
				func() error { _, err := failing.Send(ctx, email); return err },
				func() int { return len(refusingAPI.received()) + len(failingAPI.received()) }
		}},
		{"whatsapp", func(t *testing.T, cb CircuitBreaker) (func() error, func() error, func() int) {
			refusingAPI := &fakeWhatsAppCloudAPIClient{sendErr: permanent(errors.New("(#132001) template does not exist"))}
			failingAPI := &fakeWhatsAppCloudAPIClient{sendErr: errors.New("(#131000) something went wrong")}
			refusing, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: refusingAPI, CircuitBreaker: cb})
			if err != nil {
				t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
			}
			failing, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: failingAPI, CircuitBreaker: cb})
			if err != nil {
				t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
			}
			return func() error { _, err := refusing.Send(ctx, validWhatsAppMessage()); return err },
				func() error { _, err := failing.Send(ctx, validWhatsAppMessage()); return err },
				func() int { return refusingAPI.calls() + failingAPI.calls() }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb, err := NewCircuitBreaker(1, time.Hour, time.Hour)
			if err != nil {
				t.Fatalf("NewCircuitBreaker: %v", err)
			}
			refused, temporary, vendorCalls := tc.build(t, cb)
			for i := 0; i < 10; i++ {
				if err := refused(); !errors.Is(err, ErrPermanent) {
					t.Fatalf("refusal %d = %v, want a permanent failure", i, err)
				}
			}
			if cb.State() != CircuitStateClosed || vendorCalls() != 10 {
				t.Fatalf("after ten refusals: breaker %s, %d vendor calls; want closed and 10", cb.State(), vendorCalls())
			}
			if err := temporary(); err == nil || errors.Is(err, ErrPermanent) {
				t.Fatalf("the temporary failure = %v", err)
			}
			if err := refused(); !errors.Is(err, ErrCircuitOpen) {
				t.Fatalf("after a temporary failure = %v, want ErrCircuitOpen", err)
			}
			if vendorCalls() != 11 {
				t.Fatalf("%d vendor calls, want 11 (none while open)", vendorCalls())
			}
		})
	}
}
