package cancellation

import (
	ctx "context"
	"time"
)

func Deadline() {
	_, cancel := ctx.WithDeadline(ctx.Background(), time.Time{})
	defer cancel()
}

func Timeout() {
	_, cancel := ctx.WithTimeout(ctx.Background(), time.Second)
	cancel()
}

func Cancel() {
	_, cancel := ctx.WithCancel(ctx.Background())
	defer (cancel)()
}

func VarTuple() {
	var _, cancel = ctx.WithCancel(ctx.Background())
	cancel()
}

func NestedCall() {
	_, cancel := ctx.WithCancel(ctx.Background())
	func() { cancel() }()
}

func NestedTuple() {
	defer func() {
		_, cancel := ctx.WithTimeout(ctx.Background(), time.Second)
		defer cancel()
	}()
}

// A harmless shadowed local does not replace the outer variable's identity.
func DistinctLocal() {
	_, cancel := ctx.WithCancel(ctx.Background())
	{
		cancel := func() {}
		cancel()
	}
	cancel()
}

type DeadlineValue struct{ Final time.Time }

func (d DeadlineValue) cleanup() (ctx.Context, ctx.CancelFunc) {
	until := time.Now().Add(2 * time.Second)
	if d.Final.Before(until) {
		until = d.Final
	}
	return ctx.WithDeadline(ctx.Background(), until)
}

func MethodWrapper() {
	d := DeadlineValue{}
	_, cancel := d.cleanup()
	defer cancel()
}

func directWrapper() (ctx.Context, ctx.CancelFunc) {
	return ctx.WithCancel(ctx.Background())
}

func DirectWrapper() {
	_, cancel := directWrapper()
	defer cancel()
}

func ConstantLen() {
	const length = len("constant")
	_, cancel := ctx.WithCancel(ctx.Background())
	defer cancel()
	_ = length
}

func ConstantCap() {
	const capacity = cap([3]int{})
	_, cancel := ctx.WithCancel(ctx.Background())
	defer cancel()
	_ = capacity
}
