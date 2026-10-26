package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

type standaloneWorker interface {
	Run(context.Context) error
	RunChannelOutbox(context.Context) error
	RunChannelAIReplies(context.Context) error
	RunThreadsCredentialRefresh(context.Context) error
	Close() error
}

// runStandaloneWorkers uses the same independent absolute deadline owner as
// the server. Only actual loop returns count as joined. Close cannot tear down
// resources used by a loop that is still settling its durable attempt.
func runStandaloneWorkers(workers []standaloneWorker, quit <-chan os.Signal, budget time.Duration, report func(serverShutdownResult)) serverShutdownResult {
	ctx, cancel := context.WithCancel(context.Background())
	exits := make(chan struct{}, len(workers)*4)
	tasks := []serverShutdownTask{{"worker stop request", func(context.Context) error { cancel(); return nil }}}
	for i, worker := range workers {
		loops := []struct {
			name string
			run  func(context.Context) error
		}{
			{"message loop", worker.Run},
			{"outbox loop", worker.RunChannelOutbox},
			{"AI reply loop", worker.RunChannelAIReplies},
			{"Threads credential loop", worker.RunThreadsCredentialRefresh},
		}
		workerDone := make([]<-chan struct{}, 0, len(loops))
		for _, loop := range loops {
			name := fmt.Sprintf("worker %d %s", i+1, loop.name)
			done := make(chan struct{})
			var loopErr error
			go func() {
				defer close(done)
				loopErr = loop.run(ctx)
				exits <- struct{}{}
			}()
			workerDone = append(workerDone, done)
			tasks = append(tasks, serverShutdownTask{name, func(waitCtx context.Context) error {
				select {
				case <-done:
					if ctx.Err() != nil && errors.Is(loopErr, context.Canceled) {
						return nil
					}
					return loopErr
				case <-waitCtx.Done():
					return waitCtx.Err()
				}
			}})
		}
		tasks = append(tasks, serverShutdownTask{fmt.Sprintf("worker %d close after loops", i+1), func(waitCtx context.Context) error {
			for _, done := range workerDone {
				select {
				case <-done:
				case <-waitCtx.Done():
					return waitCtx.Err()
				}
			}
			return worker.Close()
		}})
	}
	// No synchronous logger/cancel/Close may precede establishing the deadline.
	select {
	case <-quit:
	case <-exits:
	}
	deadline := time.Now().Add(budget)
	return shutdownServer(deadline, tasks, report)
}
