package gmailrelay

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fencedSyncContractState struct {
	syncContractState
	commitToken  string
	commitCursor string
	commitAt     time.Time
	commitErr    error
}

func (s *fencedSyncContractState) CommitSuccessfulSync(
	_ context.Context,
	leaseToken, cursor string,
	at time.Time,
) error {
	s.commitToken = leaseToken
	s.commitCursor = cursor
	s.commitAt = at
	if s.commitErr != nil {
		return s.commitErr
	}
	s.cursor = cursor
	s.lastSuccess = at
	return nil
}

func TestGmailSyncUsesFencedAtomicCommitInProductionStyleStore(t *testing.T) {
	fixedNow := time.Date(2026, time.August, 3, 13, 0, 0, 0, time.UTC)
	state := &fencedSyncContractState{}
	gmail := &syncContractGmail{
		profile:  GmailProfile{EmailAddress: syncContractMailbox, HistoryID: "100"},
		messages: map[string]GmailRESTMessage{},
	}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, &syncContractAcceptance{})
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	syncer.now = func() time.Time { return fixedNow }

	if err := syncer.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce() error = %v", err)
	}
	if state.commitToken == "" || state.commitCursor != "100" || !state.commitAt.Equal(fixedNow) {
		t.Fatalf("fenced commit = token:%q cursor:%q at:%s", state.commitToken, state.commitCursor, state.commitAt)
	}
	if len(state.cursorWrites) != 0 || len(state.marked) != 0 {
		t.Fatalf("legacy split state writes were used: cursors=%v marks=%v", state.cursorWrites, state.marked)
	}
}

func TestGmailSyncLostLeaseCannotAdvanceCursor(t *testing.T) {
	state := &fencedSyncContractState{
		syncContractState: syncContractState{cursor: "100"},
		commitErr:         ErrSyncLeaseLost,
	}
	gmail := &syncContractGmail{
		messages: map[string]GmailRESTMessage{},
		history: func(startHistoryID, pageToken string) (GmailHistoryPage, error) {
			return GmailHistoryPage{HistoryID: "101"}, nil
		},
	}
	syncer, err := NewSyncer(syncContractConfig(), gmail, state, &syncContractAcceptance{})
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}

	err = syncer.ProcessOnce(context.Background())
	if !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("ProcessOnce() error = %v, want lost lease", err)
	}
	if state.cursor != "100" || state.commitCursor != "101" {
		t.Fatalf("lost lease changed cursor: stored=%q attempted=%q", state.cursor, state.commitCursor)
	}
}

func TestGmailSyncLeaseHeartbeatRenewsAndDetectsLostOwnership(t *testing.T) {
	state := &syncContractState{leaseHeld: true}
	syncer, err := NewSyncer(
		syncContractConfig(),
		&syncContractGmail{},
		state,
		&syncContractAcceptance{},
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if err := syncer.maintainSyncLease(ctx, "lease-token", 30*time.Millisecond); err != nil {
		t.Fatalf("maintainSyncLease() error = %v", err)
	}
	if state.renewals < 2 {
		t.Fatalf("lease renewals = %d, want at least 2", state.renewals)
	}

	state.loseOnRenew = true
	err = syncer.maintainSyncLease(context.Background(), "lease-token", 30*time.Millisecond)
	if !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("lost lease heartbeat error = %v", err)
	}
}

type fakeSyncCommitBackend struct {
	*fakeRedisBackend
	result int64
}

func (b *fakeSyncCommitBackend) CommitSuccessfulSync(
	context.Context,
	string, string, string, string, string, string,
) (int64, error) {
	return b.result, nil
}

func TestRedisStoreFencedCommitMapsLeaseAndRegressionFailures(t *testing.T) {
	config, err := loadTestConfig(validConfigEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeSyncCommitBackend{fakeRedisBackend: newFakeRedisBackend(), result: -1}
	store, err := newRedisStore(config, backend)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 3, 13, 30, 0, 0, time.UTC)

	if err := store.CommitSuccessfulSync(context.Background(), "lease-token", "12345", now); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("lost lease result = %v", err)
	}
	backend.result = -2
	if err := store.CommitSuccessfulSync(context.Background(), "lease-token", "12345", now); !errors.Is(err, ErrHistoryCursorRegression) {
		t.Fatalf("regression result = %v", err)
	}
	backend.result = 1
	if err := store.CommitSuccessfulSync(context.Background(), "lease-token", "12345", now); err != nil {
		t.Fatalf("successful fenced commit = %v", err)
	}
	if err := store.CommitSuccessfulSync(context.Background(), "lease-token", "not-a-history-id", now); err == nil {
		t.Fatal("expected malformed Gmail history cursor rejection")
	}
}
