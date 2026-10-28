# Forwarding safety qualification

This change prepares the remaining forwarding fixes on main69. It does not
rewrite historical delivery statuses, replay uncertain sends, repair data, or
change role/permission seeds.

## Admission and attachment bounds

The root App admits at most two forwards at once, including requests waiting
for their outer tenant transaction to commit. Admission does not queue. A full
instance returns `503 forward_temporarily_unavailable` before inserting a pending
message. An unstarted reservation expires after 15 seconds; its later commit
callback cannot dispatch and instead attempts the existing bounded pre-provider
failure settlement. Active workers retain admission until completion. A rollback
or lost commit callback can therefore consume a slot for at most 15 seconds.

Storage preparation still owns its existing 20-second context, upload its
30-second context, and provider delivery its 30-second context. Storage listing
failure is now a safe explicit refusal and never triggers a download. Forwarding
requires the optional `storage.BoundedObjectReader` capability for object-store
reads; the production S3 implementation supplies it. Unsupported implementations
fail closed. Each read stops at the type limit plus one byte and rejects excess;
the cap also applies to local development files. Limits remain 5 MiB for images,
16 MiB for audio/video, and 100 MiB for documents. A changed/stale advertised size
cannot bypass the read cap. The upload uses header/data/footer readers over the
existing slice, removing the second full-size multipart allocation.

Two 100 MiB documents still require substantial memory. The cap bounds retained
payloads and active jobs, not measured process RSS: Go read-buffer growth, SDK
buffers and unrelated app work remain. Hosted high-water-memory/load tests with
the deployed instance's actual headroom are required before closing
`R5-FORWARD-BOUND`. `ListPrefix` is still a paginated metadata read over an exact
object-key prefix, bounded by the storage context; replacing it with a direct
metadata/HEAD capability is a possible further reduction in metadata allocation.
This change does not add a user cancellation endpoint or change server shutdown
ownership; cancellation applies through the existing phase contexts and expired
unstarted reservations. Cross-replica capacity scales with replica count.

## Delivery truth and realtime

Copilot labels sent, delivered, read, failed, queued and unconfirmed outgoing
messages separately. Sent does not imply delivered. Provider uncertainty never
becomes an assertion that the customer received the text. SLA warning suppression
now requires a user-origin message in sent/delivered/read status; pending and
failed messages do not establish a reply. Existing API-key versus human-origin
ambiguity remains a separate tracked objective.

Local `new_message` broadcasts include only the server-derived `is_forwarded`
boolean, never the provenance metadata. The browser accepts booleans only and
keeps a missing flag undefined for rolling compatibility with older publishers.
Remote-replica identifier invalidation and canonical refetch behavior is unchanged.

## Remaining consumer work and verification

Cached contact previews still contain raw pending-attempt content written during
message insertion. Correctly labelling them requires a bounded canonical latest
message projection that includes status and reconciles updates, with account,
assignment and tenant scope preserved. Prefixing stored preview text would become
stale on confirmation and is not part of this patch. `COPILOT-FORWARD-STATUS`
therefore remains open for this derived surface. Dashboard recent messages already
include/render status, and its total count means message records, not successful
delivery. No source evidence justifies changing that count to sent-only.

New tests cover admission/expiry races, pre-write HTTP capacity refusal, permit
retention through actual provider completion, listing failure without download,
stale-size bounded reads, local read cancellation, S3 advertised/chunked oversize,
Copilot state labels, actual PostgreSQL SLA transitions, and a second agent's
forwarded socket flag. Existing multipart parsing tests now verify media bytes.
Backend, PostgreSQL and resource/load checks must execute in hosted CI; source
coverage alone is not a pass. No local Docker/database was started for this work.
