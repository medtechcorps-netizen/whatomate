# Reaform360 single-clinic pilot

Authorized Careline staff can book a Reaform appointment from a CRM lead in the
pipeline or conversation workspace. Arrival, cancellation, no-show and other
appointment status changes return automatically to that lead's appointment card.
Reaform remains the authority for availability, appointments and clinical records.

This pilot is disabled by default. It sends no messages, collects no payments and
copies no clinical notes/results. The stored appointment outcome is separate from
the lead's CRM stage: it never silently marks the sales pipeline won or lost.

## Staff workflow

1. Open the customer's lead. Select an existing Reaform patient by name or patient
   reference and explicitly confirm the masked identity details. Register new
   patients in Reaform first; shared phone numbers are not automatic identity proof.
2. Choose a date and an available doctor/time, then review the booking. The pilot
   supports 30-minute in-person appointments within the next 90 days.
3. Book once. The lead retains the appointment and the latest synchronization
   state. Changes/cancellations and attendance are managed in Reaform.
4. If a response is uncertain, use **Check booking**. The server recovers the same
   command; a timeout never creates a fresh independent request.
5. For a definite rejected booking, use the available correction action to choose
   another slot. This is offered only after Reaform confirms a durable rejected
   command. An unknown result cannot be treated as a failed booking.

The open card refreshes periodically. During an outage the last known appointment
remains visible with its last update time. A remote state update is eventual,
not a guarantee that every intermediate state transition will be recorded.

## Deployment configuration

The existing koanf environment loader uses `WHATOMATE_` as prefix and `__` for a
nested key. All values are deployment-owned, never selected by a browser request.

| Variable | Purpose |
|---|---|
| `WHATOMATE_REAFORM_PILOT__ENABLED` | Enable explicitly; default false |
| `WHATOMATE_REAFORM_PILOT__ORGANIZATION_ID` | One existing ReReply organization UUID |
| `WHATOMATE_REAFORM_PILOT__API_BASE_URL` | Public HTTPS origin of the Reaform API with a DNS hostname; no path/query/userinfo |
| `WHATOMATE_REAFORM_PILOT__API_TOKEN` | Reaform dedicated token: 64 lowercase hex characters |
| `WHATOMATE_REAFORM_PILOT__REMOTE_ORGANIZATION_ID` | Expected Reaform organization UUID |
| `WHATOMATE_REAFORM_PILOT__REMOTE_SITE_ID` | Expected Reaform clinic/site UUID |
| `WHATOMATE_REAFORM_PILOT__TERMINAL_RETENTION_DAYS` | Terminal-visit polling retention; default 90, maximum 3650 |

Keep the token only in the server secret store. The remote capabilities endpoint
must match the expected organization/site and contract version before remote
actions proceed. Redirects are refused and production HTTP transport retains its
destination restrictions. There is no runtime switch for insecure local targets.

Reaform needs its corresponding disabled-by-default configuration and migration
`0299_rereply_pilot`. See that repository's `docs/rereply-clinic-pilot.md`. Use the
normal release and migration procedures for each application; do not enable the
pilot merely because configuration values exist.

## Authorization and persistence

- Staff need the existing CRM lead permissions and access to the lead's contact.
  Read-only staff can view an authorized linked outcome, but cannot book/search.
- The integration is restricted to one ReReply organization and one Reaform
  organization/site. Requests cannot override those bindings.
- Booking intent is committed before the HTTP request. The server stores a stable
  command ID, immutable request hash, original contact and configured clinic binding.
- The existing lead metadata has one reserved server-owned namespace,
  `reaform_clinic_pilot_v1`. Generic metadata edits preserve this namespace and
  clients cannot forge it on creation. No ReReply schema migration is required.
- Linked leads cannot be casually reassigned to another contact. A changed contact
  or clinic binding stops synchronization and disclosure until reviewed.
- Row locking and bounded leases coordinate API and worker replicas. Background
  bookkeeping does not invalidate the human-editable CRM version on every poll.
- Reaform supplies a monotonic appointment revision. Stale responses and mismatched
  command/patient/source/appointment IDs are rejected rather than applied.
  An unassigned doctor is a valid state. Doctor display names are descriptive
  current labels and can refresh without an appointment revision change.
- Definitively rejected attempts are retained. Only an explicit fresh attempt
  after confirmed rejection can choose another slot. Unknown outcomes reuse the
  existing command, including after worker restart or response loss.

The synchronizer drains at most 40 due links per tick in bounded batches with
four concurrent workers and a 45-second cycle budget. It validates remote clinic
capabilities once per tick. Active and recently terminal visits are eligible
again after a minute; terminal visits older than 24 hours are checked hourly for
the configured retention period. Backlogs and outages can delay propagation.
Retention limits automatic polling, not storage: it deletes no booking history.

## Operational checks

Use synthetic records first and verify both API and worker behavior:

1. Book from a real integrated lead surface and verify exactly one Reaform visit.
2. Repeat a request or drop its response; recovery must return that same visit.
3. Create a slot conflict; select another slot after confirmed rejection. Replay
   the original rejected command and verify that it cannot later succeed.
4. Mark the exact visit arrived, cancelled or no-show through ordinary Reaform
   workflows. Verify the corresponding lead outcome and last-successful-sync time.
5. Confirm another patient's visit, an unrelated walk-in or another organization
   cannot update or reveal this lead's appointment.
6. Stop the remote service temporarily. Reopen the lead, verify its retained state,
   then restore the service and confirm automatic recovery.
7. Verify duplicate workers, stale responses and normal concurrent CRM edits
   preserve one command, the latest outcome and unrelated metadata.

Disabling the pilot stops new actions and the background synchronizer; it does
not delete the Reaform appointment or erase durable audit/command records. Restore
ordinary operations in Reaform while investigating any failed synchronization.

Confirmed clinic identities, deployed revisions, staff access and the synthetic
acceptance receipt belong in the pilot handover. A test pass is not evidence that
a particular live tenant is already activated.
