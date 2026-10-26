package worker

import "context"

// intakeContext scopes only discovery/claim statements. Provider attempts and
// accepted-result finalizers stay on the original Worker and retain their
// existing cancellation/commit-ambiguity policy. This view is never shared.
func (w *Worker) intakeContext(ctx context.Context) *Worker {
	copy := *w
	copy.DB = w.DB.WithContext(ctx)
	return &copy
}

// consumeWithHeartbeat joins the heartbeat before reporting the message loop
// finished. A stuck heartbeat remains a pending loop at the command's absolute
// deadline; cancellation alone is not reported as a successful join.
func consumeWithHeartbeat(ctx context.Context, consume func(context.Context) error, heartbeat func(context.Context)) error {
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); heartbeat(heartbeatCtx) }()
	defer func() { cancel(); <-done }()
	return consume(ctx)
}
