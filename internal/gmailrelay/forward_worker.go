package gmailrelay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/shridarpatil/whatomate/internal/metarelay"
)

type InboundQueueStore interface {
	ClaimInbound(context.Context, time.Time, time.Duration, int) ([]metarelay.InboundJob, error)
	RequeueExpired(context.Context, time.Time, int) (int, error)
	CompleteInbound(context.Context, string) error
	RetryInbound(context.Context, string, time.Time, int, string) (int, bool, error)
	DeadInbound(context.Context, string, string) error
}

type ForwardWorkerOption func(*ForwardWorker)

func WithForwardWorkerLogger(logger *slog.Logger) ForwardWorkerOption {
	return func(worker *ForwardWorker) {
		if logger != nil {
			worker.logger = logger
		}
	}
}

func WithForwardWorkerHTTPClient(client *http.Client) ForwardWorkerOption {
	return func(worker *ForwardWorker) {
		if client != nil {
			worker.client = client
		}
	}
}

type ForwardWorker struct {
	config *Config
	store  InboundQueueStore
	client *http.Client
	logger *slog.Logger
	now    func() time.Time
}

func NewForwardWorker(config *Config, store InboundQueueStore, options ...ForwardWorkerOption) (*ForwardWorker, error) {
	if config == nil || store == nil {
		return nil, errors.New("Gmail forward worker configuration is incomplete")
	}
	client := &http.Client{
		Timeout: config.ForwardTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	worker := &ForwardWorker{
		config: config,
		store:  store,
		client: client,
		logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		now:    func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		option(worker)
	}
	return worker, nil
}

func (w *ForwardWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.config.QueuePollInterval)
	defer ticker.Stop()
	for {
		if err := w.ProcessOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("Gmail relay forwarding cycle failed", "component", "inbound_forward")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *ForwardWorker) ProcessOnce(ctx context.Context) error {
	if !w.config.Paired() {
		return nil
	}
	now := w.now().UTC()
	if _, err := w.store.RequeueExpired(ctx, now, 100); err != nil {
		return err
	}
	jobs, err := w.store.ClaimInbound(ctx, now, w.config.ProcessingLease, w.config.WorkerConcurrency)
	if err != nil {
		return err
	}
	errorsCh := make(chan error, len(jobs))
	var workers sync.WaitGroup
	for _, job := range jobs {
		job := job
		workers.Add(1)
		go func() {
			defer workers.Done()
			if processErr := w.processJob(ctx, job); processErr != nil {
				errorsCh <- processErr
			}
		}()
	}
	workers.Wait()
	close(errorsCh)
	for processErr := range errorsCh {
		return processErr
	}
	return nil
}

func (w *ForwardWorker) processJob(ctx context.Context, job metarelay.InboundJob) error {
	if job.AccountKey != w.config.Mailbox {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return w.store.DeadInbound(settleCtx, job.ID, "unknown_account")
	}
	forwardCtx, cancelForward := context.WithTimeout(ctx, w.config.ForwardTimeout)
	outcome := w.forward(forwardCtx, job.Body)
	cancelForward()

	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelSettle()
	if outcome.success {
		return w.store.CompleteInbound(settleCtx, job.ID)
	}
	if !outcome.retryable {
		reason := "rereply_permanent_failure"
		if outcome.status > 0 {
			reason = "rereply_http_" + strconv.Itoa(outcome.status)
		}
		return w.store.DeadInbound(settleCtx, job.ID, reason)
	}

	delay := forwardRetryBackoff(job.Attempts + 1)
	if outcome.retryAfter > delay {
		delay = outcome.retryAfter
	}
	reason := "rereply_transport"
	if outcome.status > 0 {
		reason = "rereply_http_" + strconv.Itoa(outcome.status)
	}
	_, _, err := w.store.RetryInbound(
		settleCtx,
		job.ID,
		w.now().UTC().Add(delay),
		w.config.MaxAttempts,
		reason,
	)
	return err
}

type forwardResult struct {
	success    bool
	retryable  bool
	status     int
	retryAfter time.Duration
}

func (w *ForwardWorker) forward(ctx context.Context, body []byte) forwardResult {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		w.config.ReReplyWebhookURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return forwardResult{}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Gmail-Relay/1.0")
	request.Header.Set(ReReplySignatureHeader, signRelayBody(w.config.ReReplyInboundSecret, body))
	response, err := w.client.Do(request)
	if err != nil {
		return forwardResult{retryable: true}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return forwardResult{success: true, status: response.StatusCode}
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
		return forwardResult{
			retryable:  true,
			status:     response.StatusCode,
			retryAfter: parseProviderRetryAfter(response.Header.Get("Retry-After"), w.now()),
		}
	}
	return forwardResult{status: response.StatusCode}
}

func forwardRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 9 {
		attempt = 9
	}
	return min(time.Second<<uint(attempt-1), 5*time.Minute)
}
