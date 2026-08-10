package metarelay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metareview"
)

type WorkerOption func(*Worker)

func WithWorkerHTTPClient(client *http.Client) WorkerOption {
	return func(worker *Worker) {
		if client != nil {
			worker.client = client
		}
	}
}

func WithWorkerLogger(logger *slog.Logger) WorkerOption {
	return func(worker *Worker) {
		if logger != nil {
			worker.logger = logger
		}
	}
}

func WithWorkerReviewBindingResolver(resolver ReviewBindingResolver) WorkerOption {
	return func(worker *Worker) {
		if resolver != nil {
			worker.reviewResolver = resolver
		}
	}
}

func NewWorker(config *Config, store QueueStore, options ...WorkerOption) (*Worker, error) {
	if config == nil {
		return nil, errors.New("relay config is required")
	}
	if store == nil {
		return nil, errors.New("durable relay queue is required")
	}
	if config.WorkerConcurrency <= 0 || config.WorkerConcurrency > maxWorkerConcurrency {
		return nil, errors.New("worker concurrency is invalid")
	}
	if config.ForwardTimeout <= 0 ||
		config.ProcessingLease < config.ForwardTimeout+queueSettlementTimeout+leaseSafetyMargin {
		return nil, errors.New("processing lease does not safely cover a forwarding attempt")
	}
	worker := &Worker{
		config: config,
		store:  store,
		client: &http.Client{
			Timeout: config.ForwardTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:    time.Now,
	}
	for _, option := range options {
		option(worker)
	}
	if config.stagingMessengerReview() {
		if config.DeploymentEnvironment != "staging" {
			return nil, errors.New("Messenger review relay is staging-only")
		}
		if worker.reviewResolver == nil {
			return nil, errors.New("Messenger review binding resolver is required")
		}
		// Review callbacks contain a dynamically provisioned credential. Always
		// use the dial-time public-endpoint transport in this runtime, even if a
		// generic production HTTP client option was supplied by a caller.
		worker.client = newReviewEndpointHTTPClient(
			config.ForwardTimeout,
			config.allowInsecureTestEndpoints,
		)
	}
	return worker, nil
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.ProcessOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("Meta relay queue cycle failed", "component", "inbound_forward")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) ProcessOnce(ctx context.Context) error {
	now := w.now().UTC()
	if _, err := w.store.RequeueExpired(ctx, now, 100); err != nil {
		return err
	}
	// Claim exactly the number of jobs we can start immediately. Every claimed
	// job runs concurrently, and configuration validation guarantees that one
	// forwarding timeout plus queue settlement fits inside the lease.
	jobs, err := w.store.ClaimInbound(
		ctx,
		now,
		w.config.ProcessingLease,
		w.config.WorkerConcurrency,
	)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}

	errs := make(chan error, len(jobs))
	var workers sync.WaitGroup
	for _, job := range jobs {
		job := job
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := w.processJob(ctx, job); err != nil {
				errs <- err
			}
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

func (w *Worker) processJob(ctx context.Context, job InboundJob) error {
	if w.config.stagingMessengerReview() {
		return w.processReviewJob(ctx, job)
	}
	account, ok := w.config.accountByKey(job.AccountKey)
	if !ok {
		settleCtx, cancel := context.WithTimeout(ctx, queueSettlementTimeout)
		defer cancel()
		return w.store.DeadInbound(settleCtx, job.ID, "unknown_account")
	}

	forwardCtx, cancelForward := context.WithTimeout(ctx, w.config.ForwardTimeout)
	outcome := w.forwardToReReply(forwardCtx, account, job.Body)
	cancelForward()
	return w.settleForwardOutcome(ctx, job, account, outcome)
}

func (w *Worker) processReviewJob(ctx context.Context, job InboundJob) error {
	if job.AccountKey != w.config.ReviewChannelAccountID ||
		job.ReviewGeneration != w.config.ReviewGeneration ||
		job.ReviewCredentialID == "" || job.ReviewCredentialVersion <= 0 {
		return w.deadReviewJob(ctx, job, "review_binding_changed")
	}
	forwardCtx, cancelForward := context.WithTimeout(ctx, w.config.ForwardTimeout)
	binding, err := w.reviewResolver.ResolveReviewBinding(forwardCtx)
	if err != nil {
		cancelForward()
		if reviewBindingUnavailable(err) {
			return w.retryReviewJob(ctx, job, "review_binding_unavailable")
		}
		return w.deadReviewJob(ctx, job, "review_binding_rejected")
	}
	if err := validateReviewBinding(w.config, binding, w.now().UTC()); err != nil ||
		!reviewBindingMatchesJob(binding, job) {
		cancelForward()
		return w.deadReviewJob(ctx, job, "review_binding_changed")
	}
	account, err := binding.account()
	if err != nil {
		cancelForward()
		return w.deadReviewJob(ctx, job, "review_binding_rejected")
	}
	outcome := w.forwardReviewToReReply(forwardCtx, account, job.Body, binding)
	cancelForward()
	return w.settleForwardOutcome(ctx, job, account, outcome)
}

func (w *Worker) settleForwardOutcome(
	ctx context.Context,
	job InboundJob,
	account *AccountConfig,
	outcome forwardOutcome,
) error {

	settleCtx, cancelSettle := context.WithTimeout(ctx, queueSettlementTimeout)
	defer cancelSettle()
	if outcome.success {
		if err := w.store.CompleteInbound(settleCtx, job.ID); err != nil {
			return err
		}
		w.logger.Info(
			"Canonical Meta events accepted by ReReply",
			"account", account.Key,
			"job_id", job.ID,
		)
		return nil
	}
	if !outcome.retryable {
		reason := "rereply_permanent_failure"
		if outcome.status > 0 {
			reason = "rereply_http_" + strconv.Itoa(outcome.status)
		}
		if err := w.store.DeadInbound(settleCtx, job.ID, reason); err != nil {
			return err
		}
		w.logger.Error(
			"Canonical Meta events were dead-lettered",
			"account", account.Key,
			"job_id", job.ID,
			"status", outcome.status,
		)
		return nil
	}

	attempt := job.Attempts + 1
	delay := retryBackoff(attempt)
	if outcome.retryAfter > delay {
		delay = outcome.retryAfter
	}
	reason := "rereply_transport"
	if outcome.status > 0 {
		reason = "rereply_http_" + strconv.Itoa(outcome.status)
	}
	attempts, dead, err := w.store.RetryInbound(
		settleCtx,
		job.ID,
		w.now().UTC().Add(delay),
		w.config.MaxAttempts,
		reason,
	)
	if err != nil {
		return err
	}
	if dead {
		w.logger.Error(
			"Canonical Meta events exhausted retry budget",
			"account", account.Key,
			"job_id", job.ID,
			"attempts", attempts,
		)
	} else {
		w.logger.Warn(
			"Canonical Meta event forwarding scheduled for retry",
			"account", account.Key,
			"job_id", job.ID,
			"attempts", attempts,
			"status", outcome.status,
		)
	}
	return nil
}

func (w *Worker) deadReviewJob(ctx context.Context, job InboundJob, reason string) error {
	settleCtx, cancel := context.WithTimeout(ctx, queueSettlementTimeout)
	defer cancel()
	return w.store.DeadInbound(settleCtx, job.ID, reason)
}

func (w *Worker) retryReviewJob(ctx context.Context, job InboundJob, reason string) error {
	settleCtx, cancel := context.WithTimeout(ctx, queueSettlementTimeout)
	defer cancel()
	attempts, dead, err := w.store.RetryInbound(
		settleCtx,
		job.ID,
		w.now().UTC().Add(retryBackoff(job.Attempts+1)),
		w.config.MaxAttempts,
		reason,
	)
	if err != nil {
		return err
	}
	if dead {
		w.logger.Error(
			"Meta review event exhausted retry budget",
			"job_id", job.ID,
			"attempts", attempts,
		)
	}
	return nil
}

type forwardOutcome struct {
	success    bool
	retryable  bool
	status     int
	retryAfter time.Duration
}

func (w *Worker) forwardToReReply(
	ctx context.Context,
	account *AccountConfig,
	body []byte,
) forwardOutcome {
	return w.forwardToReReplyWithReview(ctx, account, body, nil)
}

func (w *Worker) forwardReviewToReReply(
	ctx context.Context,
	account *AccountConfig,
	body []byte,
	binding ReviewBinding,
) forwardOutcome {
	return w.forwardToReReplyWithReview(ctx, account, body, &binding)
}

func (w *Worker) forwardToReReplyWithReview(
	ctx context.Context,
	account *AccountConfig,
	body []byte,
	reviewBinding *ReviewBinding,
) forwardOutcome {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		account.ReReplyWebhookURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return forwardOutcome{}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Relay/1.0")
	request.Header.Set(ReReplySignatureHeader, signBody(account.reReplyInboundSecret, body))
	request.Header.Set(
		channelapi.RelayMetaProviderProofHeader,
		channelapi.SignMetaProviderInboundProof(w.config.ReReplyProviderProofSecret, body),
	)
	if reviewBinding != nil {
		proof, proofErr := metareview.SignInboundProof(
			w.config.ReReplyProviderProofSecret,
			reviewBinding.Tuple,
			reviewBinding.CredentialID,
			reviewBinding.CredentialVersion,
			body,
		)
		if proofErr != nil {
			return forwardOutcome{}
		}
		request.Header.Set(metareview.GenerationHeader, reviewBinding.Tuple.Generation)
		request.Header.Set(metareview.CredentialIDHeader, reviewBinding.CredentialID)
		request.Header.Set(
			metareview.CredentialVersionHeader,
			strconv.Itoa(reviewBinding.CredentialVersion),
		)
		request.Header.Set(metareview.ReviewProofHeader, proof)
	}

	response, err := w.client.Do(request)
	if err != nil {
		return forwardOutcome{retryable: true}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return forwardOutcome{success: true, status: response.StatusCode}
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
		return forwardOutcome{
			retryable:  true,
			status:     response.StatusCode,
			retryAfter: parseRetryAfter(response.Header.Get("Retry-After"), w.now().UTC()),
		}
	}
	return forwardOutcome{status: response.StatusCode}
}

func retryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 9 {
		attempt = 9
	}
	delay := time.Second << (attempt - 1)
	return min(delay, 5*time.Minute)
}

func (w *Worker) String() string {
	if w.config.stagingMessengerReview() {
		return "Meta relay worker (staging Messenger review, inbound only)"
	}
	return fmt.Sprintf("Meta relay worker (%d account mappings)", len(w.config.Accounts))
}
