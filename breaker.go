package overcurrent

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/efritz/backoff"
	"github.com/efritz/glock"
)

type (
	// CircuitBreaker protects the invocation of a function and monitors failures.
	// After a certain failure threshold is reached, future invocations will instead
	// return an ErrErrCircuitOpen instead of attempting to invoke the function again.
	CircuitBreaker interface {
		// Trip manually trips the circuit breaker. The circuit breaker will remain open
		// until it is manually reset.
		Trip()

		// Reset the circuit breaker.
		Reset()

		// ShouldTry returns true if the circuit breaker is closed or half-closed with
		// some probability. Successive calls to this method may yield different results
		// depending on the registered trip condition.
		ShouldTry() bool

		// MarkResult takes the result of the protected section and marks it as a success if
		// the error is nil or if the failure interpreter decides not to trip on this error.
		MarkResult(err error) bool

		// Call attempts to call the given function if the circuit breaker is closed, or if
		// the circuit breaker is half-closed (with some probability). Otherwise, return an
		// ErrCircuitOpen. If the function times out, the circuit breaker will fail with an
		// ErrInvocationTimeout. If the function is invoked and yields a value before the
		// timeout elapses, that value is returned.
		Call(f BreakerFunc) error

		// CallAsync invokes the given function in a goroutine, returning a channel which
		// may receive one non-nil error value and then close. The channel will close without
		// writing a value on success.
		CallAsync(f BreakerFunc) <-chan error
	}

	BreakerConfigFunc func(*circuitBreaker)
	BreakerFunc       func(ctx context.Context) error

	circuitBreaker struct {
		invocationTimeout          time.Duration
		halfClosedRetryProbability float64
		maxConcurrency             int
		maxConcurrencyTimeout      time.Duration
		resetBackoff               backoff.Backoff
		failureInterpreter         FailureInterpreter
		tripCondition              TripCondition
		collector                  MetricCollector
		clock                      glock.Clock
		mutex                      sync.RWMutex
		state                      CircuitState
		lastFailureTime            *time.Time
		resetTimeout               *time.Duration
	}

	CircuitState int
)

const (
	_               CircuitState = iota
	StateOpen                    // Failure state
	StateClosed                  // Success state
	StateHalfClosed              // Cautious, probabilistic retry state
	StateHardOpen                // Forced failure state
)

var (
	// ErrCircuitOpen occurs when the Call method fails immediately.
	ErrCircuitOpen = fmt.Errorf("circuit is open")

	// ErrInvocationTimeout occurs when the method takes too long to execute.
	ErrInvocationTimeout = fmt.Errorf("invocation has timed out")
)

// NewCircuitBreaker creates a new CircuitBreaker.
func NewCircuitBreaker(configs ...BreakerConfigFunc) CircuitBreaker {
	return newCircuitBreaker(configs...)
}

func newCircuitBreaker(configs ...BreakerConfigFunc) *circuitBreaker {
	breaker := &circuitBreaker{
		invocationTimeout:          100 * time.Millisecond,
		halfClosedRetryProbability: 0.5,
		maxConcurrency:             100,
		maxConcurrencyTimeout:      time.Millisecond * 100,
		resetBackoff:               backoff.NewConstantBackoff(1000 * time.Millisecond),
		failureInterpreter:         NewAnyErrorFailureInterpreter(),
		tripCondition:              NewConsecutiveFailureTripCondition(5),
		collector:                  defaultCollector,
		clock:                      glock.NewRealClock(),
	}

	for _, config := range configs {
		config(breaker)
	}

	breaker.collector.ReportNew(BreakerConfig{
		MaxConcurrency: breaker.maxConcurrency,
	})

	breaker.setState(StateClosed)
	return breaker
}

func WithInvocationTimeout(timeout time.Duration) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.invocationTimeout = timeout }
}

func WithHalfClosedRetryProbability(probability float64) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.halfClosedRetryProbability = probability }
}

func WithResetBackoff(resetBackoff backoff.Backoff) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.resetBackoff = resetBackoff }
}

func WithFailureInterpreter(failureInterpreter FailureInterpreter) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.failureInterpreter = failureInterpreter }
}

func WithTripCondition(tripCondition TripCondition) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.tripCondition = tripCondition }
}

func WithMaxConcurrency(maxConcurrency int) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.maxConcurrency = maxConcurrency }
}

func WithMaxConcurrencyTimeout(timeout time.Duration) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.maxConcurrencyTimeout = timeout }
}

func WithCollector(collector MetricCollector) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.collector = collector }
}

func withClock(clock glock.Clock) BreakerConfigFunc {
	return func(cb *circuitBreaker) { cb.clock = clock }
}

//
// Breaker Implementation

func (cb *circuitBreaker) Trip() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.setState(StateHardOpen)
}

func (cb *circuitBreaker) Reset() {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.setState(StateClosed)
	cb.resetTimeout = nil
	cb.resetBackoff.Reset()
	cb.tripCondition.Success()
}

func (cb *circuitBreaker) ShouldTry() bool {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.state == StateHardOpen {
		return false
	}

	if !cb.tripCondition.ShouldTrip() {
		cb.setState(StateClosed)
		return true
	}

	if cb.state == StateClosed {
		cb.resetBackoff.Reset()
	}

	if cb.state != StateOpen {
		reset := cb.resetBackoff.NextInterval()
		cb.resetTimeout = &reset
	}

	if cb.resetTimeoutElapsed() {
		cb.setState(StateHalfClosed)
		return rand.Float64() < cb.halfClosedRetryProbability
	}

	cb.setState(StateOpen)
	return false
}

func (cb *circuitBreaker) MarkResult(err error) bool {
	if err != nil && (err == ErrInvocationTimeout || cb.failureInterpreter.ShouldTrip(err)) {
		cb.mutex.Lock()
		defer cb.mutex.Unlock()

		now := cb.clock.Now()
		cb.lastFailureTime = &now
		cb.tripCondition.Failure()
		return false
	}

	cb.Reset()
	return true
}

func (cb *circuitBreaker) Call(f BreakerFunc) error {
	if !cb.ShouldTry() {
		cb.collector.ReportCount(EventTypeShortCircuit)
		return ErrCircuitOpen
	}

	start := time.Now()
	err := callWithTimeout(f, cb.clock, cb.invocationTimeout)
	elapsed := time.Now().Sub(start)

	cb.collector.ReportDuration(EventTypeRunDuration, elapsed)

	if !cb.MarkResult(err) {
		if err == ErrInvocationTimeout {
			cb.collector.ReportCount(EventTypeTimeout)
		} else {
			cb.collector.ReportCount(EventTypeError)
		}
	} else if err != nil {
		cb.collector.ReportCount(EventTypeBadRequest)
	}

	return err
}

func (cb *circuitBreaker) CallAsync(f BreakerFunc) <-chan error {
	return toErrChan(func() error {
		return cb.Call(f)
	})
}

//
// Internal Methods

func (cb *circuitBreaker) setState(state CircuitState) {
	if cb.state != state {
		cb.state = state
		cb.collector.ReportState(state)
	}
}

func (cb *circuitBreaker) resetTimeoutElapsed() bool {
	if cb.state != StateOpen {
		return false
	}

	if cb.lastFailureTime == nil || cb.resetTimeout == nil {
		return false
	}

	return cb.clock.Now().Sub(*cb.lastFailureTime) >= *cb.resetTimeout
}

func callWithTimeout(f BreakerFunc, clock glock.Clock, timeout time.Duration) error {
	if timeout == 0 {
		return f(context.Background())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := toErrChan(func() error {
		return f(ctx)
	})

	select {
	case err := <-ch:
		return err

	case <-clock.After(timeout):
		return ErrInvocationTimeout
	}
}
