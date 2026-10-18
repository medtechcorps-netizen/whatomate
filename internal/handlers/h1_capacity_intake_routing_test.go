package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestH1IntakeCohort_RealIngressUsesIntakeAfterDiscoveryAndKeepsPreparationOrdinary(t *testing.T) {
	for _, route := range []string{"classic", "coexistence"} {
		t.Run(route, func(t *testing.T) {
			app, shared, pool, account := h1IngressFixture(t, route == "coexistence")
			g := &app.continuationAdmission
			before := h1IngressCounts(t, shared, account.OrganizationID)
			hold := h1OrgHold(t, g, 1, inboundContinuationPerformPriority, func() int { return 10 }, account.OrganizationID.String(), inboundContinuationWriter)
			processor := NewInboundContinuationProcessor(app, time.Second)
			ctx, cancelPreparation := context.WithCancel(context.Background())
			preparation, preparationJoined := make(chan h1CapacityAdmissionResult, 1), make(chan struct{})
			type result struct {
				work      *persistedIncomingMessage
				duplicate bool
				err       error
			}
			done, joined := make(chan result, 1), make(chan struct{})
			started := false
			t.Cleanup(func() {
				cancelPreparation()
				hold()
				select {
				case <-preparationJoined:
				case <-time.After(5 * time.Second):
					t.Error("ordinary preparation admission did not join")
				}
				// Release a granted but unconsumed preparation before joining the
				// ingress worker; failure-path cleanup must not retain its permit.
				select {
				case pending := <-preparation:
					if pending.release != nil {
						pending.release()
					}
				default:
				}
				if started {
					select {
					case <-joined:
					case <-time.After(30 * time.Second):
						t.Error("real intake admission did not join")
					}
				}
				h1IngressJoin(t, app)
			})
			go func() {
				defer close(preparationJoined)
				release, err := processor.acquirePreparationCapacity(ctx, account.OrganizationID)
				preparation <- h1CapacityAdmissionResult{release: release, err: err}
			}()
			h1CapacityWaitQueue(t, g, 1, 1)
			message := inboundContinuationTextMessage(t, "wamid.intake-routing-"+uuid.NewString(), coexistenceAdmissionTestPhone(), "Synthetic intake route")
			persist := func() result {
				var r result
				if route == "coexistence" {
					r.work, r.duplicate, r.err = app.persistAuthenticatedIncomingMessageBeforeAck(account.PhoneID, message, "Synthetic intake", strings.Repeat("a", 64))
				} else {
					r.work, r.duplicate, r.err = app.persistIncomingMessageBeforeAck(account.PhoneID, message, "Synthetic intake")
				}
				return r
			}
			started = true
			go func() { defer close(joined); done <- persist() }()
			require.Eventually(t, func() bool {
				g.mu.Lock()
				defer g.mu.Unlock()
				if g.used != 1 || len(g.waiters) != 2 || pool.Stats().InUse != 0 {
					return false
				}
				ordinary, intake := g.waiters[0], g.waiters[1]
				return !ordinary.intake && intake.intake &&
					ordinary.organization == account.OrganizationID.String() && intake.organization == account.OrganizationID.String() &&
					ordinary.mode == inboundContinuationReader && intake.mode == inboundContinuationWriter &&
					ordinary.priority == inboundContinuationFreshPriority && intake.priority == inboundContinuationFreshPriority
			}, 5*time.Second, time.Millisecond, "actual discovery must release SQL/credit, then queue only ingress as intake")
			require.Equal(t, before, h1IngressCounts(t, shared, account.OrganizationID))
			hold()
			select {
			case got := <-done:
				require.NoError(t, got.err)
				require.NotNil(t, got.work)
				require.Equal(t, account.OrganizationID, got.work.OrganizationID)
				require.False(t, got.duplicate)
			case <-time.After(10 * time.Second):
				t.Fatal("captured real intake did not precede the older preparation")
			}
			p := h1OrgGrant(t, preparation)
			p()
			h1IngressJoin(t, app)
			after := h1IngressCounts(t, shared, account.OrganizationID)
			require.Equal(t, before["messages"]+1, after["messages"])
			require.Equal(t, before["jobs"]+1, after["jobs"])
			replayed := persist()
			require.NoError(t, replayed.err)
			require.True(t, replayed.duplicate)
			h1IngressJoin(t, app)
			require.Equal(t, after, h1IngressCounts(t, shared, account.OrganizationID))
			h1OrgEmpty(t, g)
		})
	}
}
