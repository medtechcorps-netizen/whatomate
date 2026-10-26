package handlers

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const forwardMaxConcurrent = 2

var errForwardCapacity = errors.New("forward capacity exhausted")

// Admission includes reservations awaiting the outer tenant COMMIT. There is
// no queue and no worker starts until that commit's callback runs. A discarded
// callback (rollback or ambiguous commit) cannot leak capacity indefinitely.
type forwardAdmission struct {
	once  sync.Once
	slots chan struct{}
}

type forwardWorkLease struct {
	state atomic.Uint32 // 0 reserved, 1 running, 2 released, 3 expired before start
	slots chan struct{}
	timer *time.Timer
}

func (a *App) reserveForward() (*forwardWorkLease, error) {
	admission := &a.rootApp().forwardAdmission
	admission.once.Do(func() { admission.slots = make(chan struct{}, forwardMaxConcurrent) })
	select {
	case admission.slots <- struct{}{}:
		lease := &forwardWorkLease{slots: admission.slots}
		lease.timer = time.AfterFunc(forwardRequestTimeout, lease.expireReservation)
		return lease, nil
	default:
		return nil, errForwardCapacity
	}
}

func (l *forwardWorkLease) expireReservation() {
	if l.state.CompareAndSwap(0, 3) {
		<-l.slots
	}
}

func (l *forwardWorkLease) start() bool {
	if !l.state.CompareAndSwap(0, 1) {
		return false
	}
	l.timer.Stop()
	return true
}

func (l *forwardWorkLease) release() {
	if l == nil {
		return
	}
	l.timer.Stop()
	if previous := l.state.Swap(2); previous == 0 || previous == 1 {
		<-l.slots
	}
}
