package overcurrent

import (
	"errors"
	"math/rand"
	"time"

	"github.com/efritz/backoff"
)

type (
	CircuitState int
	Backoff      backoff.Backoff
)

const (
	OpenState       CircuitState = iota // Failure state
	ClosedState                         // Success state
	HalfClosedState                     // Cautious, probabilistic retry state
)

var (
	CircuitOpenError       = errors.New("Circuit is open.")
	InvocationTimeoutError = errors.New("Invocation has timed out.")
)

//
//

// A CircuitBreaker protects the invocation of a function and monitors failures.
// After a certain failure threshold is reached, future invocations will instead
// return a CircuitOpenError instead of attempting to invoke the function again.
type CircuitBreaker struct {
	config          *CircuitBreakerConfig
	hardTrip        bool
	lastState       CircuitState
	lastFailureTime *time.Time
	resetTimeout    *time.Duration
}

func NewCircuitBreaker(config *CircuitBreakerConfig) *CircuitBreaker {
	return &CircuitBreaker{
		config:    config,
		lastState: ClosedState,
	}
}

// Attempt to call the given function if the circuit breaker is closed, or if
// the circuit breaker is half-closed (with some probability). Otherwise, return
// a CircuitOpenError. If the function times out, the circuit breaker will fail
// with a InvocationTimeoutError. If the function is invoked and yields an error
// value, that value is returned.
func (cb *CircuitBreaker) Call(f func() error) error {
	if !cb.shouldTry() {
		return CircuitOpenError
	}

	if err := cb.callWithTimeout(f); err != nil && cb.config.FailureInterpreter.ShouldTrip(err) {
		cb.recordFailure()
		return err
	}

	cb.Reset()
	return nil
}

// Manually trip the circuit breaker. The circuit breaker will remain open until
// it is manually reset.
func (cb *CircuitBreaker) Trip() {
	cb.hardTrip = true
	cb.recordFailure()
}

// Reset the circuit breaker.
func (cb *CircuitBreaker) Reset() {
	cb.recordSuccess()
}

func (cb *CircuitBreaker) shouldTry() bool {
	if cb.hardTrip || cb.state() == OpenState {
		return false
	}

	if cb.state() == HalfClosedState {
		return rand.Float64() <= cb.config.HalfClosedRetryProbability
	}

	return true
}

func (cb *CircuitBreaker) state() CircuitState {
	if !cb.config.TripCondition.ShouldTrip() {
		cb.lastState = ClosedState
		return cb.lastState
	}

	if cb.lastState == ClosedState {
		cb.config.ResetBackoff.Reset()
	}

	if cb.lastState != OpenState {
		cb.updateBackoff()
	}

	if cb.lastFailureTime != nil {
		if time.Now().Sub(*cb.lastFailureTime) >= *cb.resetTimeout {
			cb.lastState = HalfClosedState
			return HalfClosedState
		}
	}

	cb.lastState = OpenState
	return OpenState
}

func (cb *CircuitBreaker) callWithTimeout(f func() error) error {
	if cb.config.InvocationTimeout == 0 {
		return f()
	}

	c := make(chan error)
	go func() {
		c <- f()
		close(c)
	}()

	select {
	case err := <-c:
		return err
	case <-time.After(cb.config.InvocationTimeout):
		return InvocationTimeoutError
	}
}

func (cb *CircuitBreaker) recordSuccess() {
	cb.hardTrip = false
	cb.lastFailureTime = nil
	cb.resetTimeout = nil

	cb.config.ResetBackoff.Reset()
	cb.config.TripCondition.Success()
}

func (cb *CircuitBreaker) recordFailure() {
	now := time.Now()
	cb.lastFailureTime = &now
	cb.config.TripCondition.Failure()
}

func (cb *CircuitBreaker) updateBackoff() {
	reset := cb.config.ResetBackoff.NextInterval()
	cb.resetTimeout = &reset
}
