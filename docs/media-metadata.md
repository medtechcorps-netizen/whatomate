# Bounded object metadata observations

`storageprobe.Reader` is a separate read-only interface, implemented by
`storageprobe.Client.Head`. It sends a metadata-only HEAD for the exact supplied object key,
with one SDK attempt, no redirects, a 16 KiB response-header limit and a five-second
deadline clipped to the caller's earlier deadline. Its temporary transport uses
the SDK defaults for system trust and proxy settings and closes idle connections.
Construct it with `storageprobe.NewClient` and the accepted storage configuration.
Normal media read/write clients retain their existing behavior.

Success contains object size, last-modified time and the provider ETag, without
downloading content. The ETag is opaque; it is not asserted to be a content hash.
Incomplete metadata, network/provider failures and invalid keys return no partial
object information. Error messages omit provider text, URLs and object keys.
Context cancellation and deadline errors remain distinguishable.

`ErrObjectMetadataNotFound` means that the configured endpoint answered 404.
It deliberately differs from the content reader's `ErrObjectNotFound`. HEAD
errors can be generic; a 404 does not establish a bucket's identity, historical
Tigris ownership, or authority to delete a reference. Authorization failure has a
separate error; other failures stay unavailable. See the
[AWS HeadObject contract](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html).

The caller must establish tenant/account scope, accepted Spaces configuration,
exact key provenance, finite inventory bounds and safe error handling. This
primitive grants no access and performs no deletion or database changes. Full
retirement still requires a complete selected-reference manifest. Known attachment
families include message envelopes, message parts, campaign previews, call
recordings and staged inbound media. This list is not exhaustive. Recordings use
`recordings/<organization>/<call>.ogg`; message and campaign keys instead use
`organizations/<organization>/...`. An object prefix scan is not a database
reference inventory. Provider media IDs and Meta template header handles are not
object keys.

Protect IVR configuration, organization hold/ringback audio, catalog artwork,
contact avatars, reseller branding and recovery artifacts. IVR `audio_file` and
organization calling filenames refer to local audio storage. Recovery coordinates
belong to recovery services; an attachment inventory must not expose encryption
key references, backup content or full organization settings.

Reserved identity-review inbound events may be protected by an immutable-event
guard. Do not clear those payloads, delete receipts or bypass integrity triggers.
Establish live visibility and the installed contract before preparing a narrowly
scoped retirement operation; a source audit is not runtime proof.

Close the bounded database snapshot before metadata network calls. Bound total
probe count, duration and private output separately; a truncated batch is not a
complete inventory. Recheck exact references, generations and concurrent
hydration immediately before any retirement. Currently working Spaces objects,
protected or unresolved references, and unrelated customer text/state must be
preserved. A selected-account inventory cannot authorize provider-wide deletion.
