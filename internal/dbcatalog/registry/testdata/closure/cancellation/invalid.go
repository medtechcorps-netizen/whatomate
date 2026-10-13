package cancellation

import (
	"context"
	"time"
)

func Reassigned() {
	_, cancel := context.WithCancel(context.Background())
	cancel = func() {}
	cancel()
}

func NestedReassigned() {
	_, cancel := context.WithCancel(context.Background())
	func() { cancel = func() {} }()
	cancel()
}

func AddressEscape() {
	_, cancel := context.WithCancel(context.Background())
	pointer := &cancel
	_ = pointer
	cancel()
}

func NestedAddressEscape() {
	_, cancel := context.WithCancel(context.Background())
	func() { pointer := &cancel; _ = pointer }()
	cancel()
}

func Alias() {
	_, cancel := context.WithCancel(context.Background())
	alias := cancel
	alias()
	cancel()
}

func ReturnValue() context.CancelFunc {
	_, cancel := context.WithCancel(context.Background())
	cancel()
	return cancel
}

func StoredValue() {
	_, cancel := context.WithCancel(context.Background())
	stored := []context.CancelFunc{cancel}
	_ = stored
	cancel()
}

func apply(callback context.CancelFunc) { callback() }
func CallbackEscape() {
	_, cancel := context.WithCancel(context.Background())
	apply(cancel)
	cancel()
}

func InitializedLater() {
	var cancel context.CancelFunc
	_, cancel = context.WithCancel(context.Background())
	cancel()
}

func WrongTupleSlot() {
	_, original := context.WithCancel(context.Background())
	cancel := original
	cancel()
}

func FactoryAlias() {
	factory := context.WithCancel
	_, cancel := factory(context.Background())
	cancel()
}

func WrongFactory() {
	_, cancel := context.WithCancelCause(context.Background())
	cancel(nil)
}

func fakeFactory(context.Context) (context.Context, context.CancelFunc) {
	return context.Background(), func() {}
}

func ShadowedFactory() {
	background := context.Background()
	context := struct {
		WithCancel func(context.Context) (context.Context, context.CancelFunc)
	}{fakeFactory}
	_, cancel := context.WithCancel(background)
	cancel()
}

func namedWrapper() (result context.Context, cancel context.CancelFunc) {
	defer func() { cancel = func() {} }()
	return context.WithCancel(context.Background())
}
func NamedWrapper() { _, cancel := namedWrapper(); cancel() }

func mixedWrapper(choice bool) (context.Context, context.CancelFunc) {
	if choice {
		return context.WithCancel(context.Background())
	}
	return context.Background(), func() {}
}
func MixedWrapper() { _, cancel := mixedWrapper(true); cancel() }

func chainedWrapper() (context.Context, context.CancelFunc) { return directWrapper() }
func ChainedWrapper()                                       { _, cancel := chainedWrapper(); cancel() }

func indirectWrapper() (context.Context, context.CancelFunc) {
	factory := context.WithCancel
	return factory(context.Background())
}
func IndirectWrapper() { _, cancel := indirectWrapper(); cancel() }

func nestedWrapper() (context.Context, context.CancelFunc) {
	func() {}()
	return context.WithCancel(context.Background())
}
func NestedWrapper() { _, cancel := nestedWrapper(); cancel() }

type cleanupInterface interface {
	cleanup() (context.Context, context.CancelFunc)
}

func InterfaceWrapper(value cleanupInterface) { _, cancel := value.cleanup(); cancel() }

func MethodExpression() {
	_, cancel := DeadlineValue.cleanup(DeadlineValue{})
	cancel()
}

func WrapperAlias() {
	factory := directWrapper
	_, cancel := factory()
	cancel()
}

func RangeReassigned() {
	_, cancel := context.WithCancel(context.Background())
	for _, cancel = range []context.CancelFunc{func() {}} {
	}
	cancel()
}

func OtherFunctionType() {
	var cancel func()
	_, cancel = context.WithTimeout(context.Background(), time.Second)
	cancel()
}
