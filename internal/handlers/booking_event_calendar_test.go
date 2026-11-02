package handlers

import (
	"sort"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestBookingEventCalendarOverlapAndLegacyBounds(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	service, resource := createBookingAvailabilityTestServiceAndResource(t, db, org.ID, user.ID)
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)
	makeEvent := func(start, end time.Time) models.BookingEvent {
		return createBookingAvailabilityTestEvent(t, db, org.ID, user.ID, service.ID, resource.ID, start, end, models.BookingEventStatusScheduled)
	}
	overnight := makeEvent(from.Add(-30*time.Minute), from.Add(30*time.Minute))
	spanning := makeEvent(from.AddDate(0, 0, -30), to.AddDate(0, 0, 30))
	inside := makeEvent(from.Add(time.Hour), from.Add(2*time.Hour))
	makeEvent(from.Add(-time.Hour), from)
	touchingEnd := makeEvent(to, to.Add(time.Hour))
	foreign := testutil.CreateTestOrganization(t, db)
	foreignUser := testutil.CreateTestUser(t, db, foreign.ID, testutil.WithSuperAdmin())
	foreignService, foreignResource := createBookingAvailabilityTestServiceAndResource(t, db, foreign.ID, foreignUser.ID)
	createBookingAvailabilityTestEvent(t, db, foreign.ID, foreignUser.ID, foreignService.ID, foreignResource.ID, from, to, models.BookingEventStatusScheduled)

	list := func(params map[string]string, status int) []BookingEventResponse {
		t.Helper()
		r := testutil.NewGETRequest(t)
		testutil.SetAuthContext(r, org.ID, user.ID)
		for key, value := range params {
			r.RequestCtx.QueryArgs().Set(key, value)
		}
		require.NoError(t, app.ListBookingEvents(r))
		require.Equal(t, status, testutil.GetResponseStatusCode(r))
		if status != fasthttp.StatusOK {
			return nil
		}
		var result struct {
			Events []BookingEventResponse `json:"events"`
		}
		testutil.ParseEnvelopeResponse(t, r, &result)
		return result.Events
	}
	f, e := from.Format(time.RFC3339), to.Format(time.RFC3339)
	rows := list(map[string]string{"overlap_from": f, "overlap_to": e}, fasthttp.StatusOK)
	require.Len(t, rows, 3)
	require.Equal(t, spanning.ID, rows[0].ID)
	require.Equal(t, overnight.ID, rows[1].ID)
	require.Equal(t, inside.ID, rows[2].ID)
	legacy := list(map[string]string{"from": f, "to": e}, fasthttp.StatusOK)
	require.Len(t, legacy, 2)
	require.Equal(t, inside.ID, legacy[0].ID)
	require.Equal(t, touchingEnd.ID, legacy[1].ID)
	require.Empty(t, list(map[string]string{"overlap_from": f, "overlap_to": e, "resource_id": foreignResource.ID.String()}, fasthttp.StatusOK))
	require.Len(t, list(map[string]string{"overlap_from": f, "overlap_to": e, "service_id": service.ID.String(), "resource_id": resource.ID.String()}, fasthttp.StatusOK), 3)
	for name, params := range map[string]map[string]string{
		"missing end":        {"overlap_from": f},
		"missing start":      {"overlap_to": e},
		"empty":              {"overlap_from": "", "overlap_to": e},
		"invalid":            {"overlap_from": "not-a-time", "overlap_to": e},
		"equal":              {"overlap_from": f, "overlap_to": f},
		"reversed":           {"overlap_from": e, "overlap_to": f},
		"legacy mixed":       {"overlap_from": f, "overlap_to": e, "from": f},
		"legacy empty mixed": {"overlap_from": f, "overlap_to": e, "to": ""},
	} {
		t.Run(name, func(t *testing.T) { list(params, fasthttp.StatusBadRequest) })
	}
}

func TestBookingEventCalendarSameStartPagination(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	service, resource := createBookingAvailabilityTestServiceAndResource(t, db, org.ID, user.ID)
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	var expected []string
	for i := 0; i < 101; i++ {
		person := resource
		if i > 0 {
			person = createBookingAvailabilityTestResource(t, db, org.ID, user.ID, "Same-start person")
		}
		event := createBookingAvailabilityTestEvent(t, db, org.ID, user.ID, service.ID, person.ID, start, start.Add(time.Hour), models.BookingEventStatusScheduled)
		expected = append(expected, event.ID.String())
	}
	sort.Strings(expected)
	var actual []string
	for _, page := range []string{"1", "2"} {
		r := testutil.NewGETRequest(t)
		testutil.SetAuthContext(r, org.ID, user.ID)
		r.RequestCtx.QueryArgs().Set("overlap_from", start.Format(time.RFC3339))
		r.RequestCtx.QueryArgs().Set("overlap_to", start.Add(time.Hour).Format(time.RFC3339))
		r.RequestCtx.QueryArgs().Set("page", page)
		r.RequestCtx.QueryArgs().Set("limit", "100")
		require.NoError(t, app.ListBookingEvents(r))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r))
		var result struct {
			Events []BookingEventResponse `json:"events"`
			Total  int                    `json:"total"`
		}
		testutil.ParseEnvelopeResponse(t, r, &result)
		require.Equal(t, 101, result.Total)
		for _, event := range result.Events {
			actual = append(actual, event.ID.String())
		}
	}
	require.Equal(t, expected, actual)
}
