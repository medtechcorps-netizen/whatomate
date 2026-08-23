export const LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX =
  'rereply:legacy-whatsapp-reply-attempt:v1:'

interface StoredLegacyWhatsAppReplyAttempt {
  key: string
  randomSalt: string
  bodySha256: string
  createdAt: number
  serviceWindowEndsAt: string
}

interface AttemptScope {
  organizationId: string
  conversationId: string
}

interface AttemptInput extends AttemptScope {
  body: string
  serviceWindowEndsAt: string
  now?: number
}

export class LegacyWhatsAppReplyAttemptStorageError extends Error {
  constructor() {
    super(
      'Secure WhatsApp retry storage is unavailable. Open native Chat to send this reply safely.',
    )
    this.name = 'LegacyWhatsAppReplyAttemptStorageError'
  }
}

// If an identity-bound namespace could not be removed and verified, direct
// replies remain disabled. This flag contains no customer data and exists only
// to prevent a later identity from trusting a potentially stale attempt.
let namespaceIsolationVerified = true

function storageKey({ organizationId, conversationId }: AttemptScope) {
  return `${LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX}${encodeURIComponent(organizationId)}:${encodeURIComponent(conversationId)}`
}

function requireSessionStore(): Storage {
  if (typeof window === 'undefined') {
    throw new LegacyWhatsAppReplyAttemptStorageError()
  }
  try {
    const storage = window.sessionStorage
    // Accessing the property can succeed while one or more operations are
    // blocked or silently ignored. Verify the complete durability contract
    // before an endpoint call can reuse or create an idempotency key.
    const probeKey = `${LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX}storage-probe`
    const probeValue = 'verified'
    storage.setItem(probeKey, probeValue)
    if (storage.getItem(probeKey) !== probeValue) {
      throw new Error('sessionStorage probe write was not persisted')
    }
    storage.removeItem(probeKey)
    if (storage.getItem(probeKey) !== null) {
      throw new Error('sessionStorage probe removal was not persisted')
    }
    return storage
  } catch {
    throw new LegacyWhatsAppReplyAttemptStorageError()
  }
}

function isStoredAttempt(value: unknown): value is StoredLegacyWhatsAppReplyAttempt {
  if (!value || typeof value !== 'object') return false
  const record = value as Record<string, unknown>
  const serviceWindowEnd =
    typeof record.serviceWindowEndsAt === 'string'
      ? Date.parse(record.serviceWindowEndsAt)
      : Number.NaN
  return (
    Object.keys(record).length === 5 &&
    typeof record.key === 'string' &&
    record.key.length > 0 &&
    record.key.length <= 255 &&
    typeof record.randomSalt === 'string' &&
    /^[a-f0-9]{32}$/.test(record.randomSalt) &&
    typeof record.bodySha256 === 'string' &&
    /^[a-f0-9]{64}$/.test(record.bodySha256) &&
    typeof record.createdAt === 'number' &&
    Number.isFinite(record.createdAt) &&
    Number.isFinite(serviceWindowEnd) &&
    new Date(serviceWindowEnd).toISOString() === record.serviceWindowEndsAt
  )
}

function readSerialized(storage: Storage, key: string) {
  try {
    return storage.getItem(key)
  } catch {
    throw new LegacyWhatsAppReplyAttemptStorageError()
  }
}

function verifyRemove(storage: Storage, key: string) {
  try {
    storage.removeItem(key)
    if (storage.getItem(key) !== null) {
      throw new Error('sessionStorage removal was not persisted')
    }
  } catch {
    throw new LegacyWhatsAppReplyAttemptStorageError()
  }
}

function readAttempt(
  storage: Storage,
  key: string,
): StoredLegacyWhatsAppReplyAttempt | null {
  const serialized = readSerialized(storage, key)
  if (!serialized) return null
  try {
    const parsed: unknown = JSON.parse(serialized)
    if (isStoredAttempt(parsed)) return parsed
  } catch {
    // Fall through to the fail-closed integrity error below.
  }
  // An invalid record may still represent an ambiguous request that reached
  // Meta. Silently deleting it and minting a new key could duplicate a send.
  throw new LegacyWhatsAppReplyAttemptStorageError()
}

function writeAttempt(
  storage: Storage,
  key: string,
  attempt: StoredLegacyWhatsAppReplyAttempt,
) {
  const serialized = JSON.stringify(attempt)
  try {
    storage.setItem(key, serialized)
    if (storage.getItem(key) !== serialized) {
      throw new Error('sessionStorage write was not persisted')
    }
  } catch {
    throw new LegacyWhatsAppReplyAttemptStorageError()
  }
}

function removeAttempt(storage: Storage, key: string, expectedKey?: string) {
  const stored = readAttempt(storage, key)
  if (!stored) return
  if (expectedKey && stored.key !== expectedKey) return
  verifyRemove(storage, key)
}

function hexToBytes(hex: string) {
  const bytes = new Uint8Array(hex.length / 2)
  for (let index = 0; index < bytes.length; index += 1) {
    bytes[index] = Number.parseInt(hex.slice(index * 2, index * 2 + 2), 16)
  }
  return bytes
}

function bytesToHex(bytes: Uint8Array) {
  return Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
}

function createRandomSalt() {
  const bytes = new Uint8Array(16)
  crypto.getRandomValues(bytes)
  return bytesToHex(bytes)
}

async function bodySha256(randomSalt: string, body: string) {
  const saltBytes = hexToBytes(randomSalt)
  const bodyBytes = new TextEncoder().encode(body)
  const input = new Uint8Array(saltBytes.length + bodyBytes.length)
  input.set(saltBytes)
  input.set(bodyBytes, saltBytes.length)
  const digest = await crypto.subtle.digest('SHA-256', input)
  return bytesToHex(new Uint8Array(digest))
}

function ensureNamespaceIsolation() {
  if (namespaceIsolationVerified) return
  clearLegacyWhatsAppReplyAttemptNamespace()
  if (!namespaceIsolationVerified) {
    throw new LegacyWhatsAppReplyAttemptStorageError()
  }
}

export async function getOrCreateLegacyWhatsAppReplyAttempt(input: AttemptInput) {
  const now = input.now ?? Date.now()
  const serviceWindowEnd = Date.parse(input.serviceWindowEndsAt)
  const key = storageKey(input)
  if (
    !input.organizationId ||
    !input.conversationId ||
    !Number.isFinite(serviceWindowEnd) ||
    serviceWindowEnd <= now
  ) {
    // Do not mutate an unresolved attempt based on malformed or stale view
    // metadata. A later canonical open window will replace it safely after a
    // verified removal.
    throw new Error('WhatsApp service window is not available for this reply')
  }
  const canonicalServiceWindowEndsAt = new Date(serviceWindowEnd).toISOString()

  ensureNamespaceIsolation()
  const storage = requireSessionStore()
  const stored = readAttempt(storage, key)
  if (stored) {
    const storedServiceWindowEnd = Date.parse(stored.serviceWindowEndsAt)
    if (storedServiceWindowEnd < now) {
      // The attempt's original service window has expired and the validated
      // current metadata advertises a later open window. Reusing the previous
      // key would make the backend replay its old row and suppress a legitimate
      // send in the newly opened window.
      verifyRemove(storage, key)
    } else {
      // An inbound message may extend the current end before the attempt's
      // original window expires. Keep the ambiguous key through that original
      // expiry so a retry cannot duplicate a request that already reached Meta.
      const digest = await bodySha256(stored.randomSalt, input.body)
      if (digest === stored.bodySha256) {
        return { ...stored, reused: true as const }
      }

      // The draft changed. The unresolved old logical attempt must be removed
      // and that removal verified before a replacement key may be sent.
      verifyRemove(storage, key)
    }
  }

  const randomSalt = createRandomSalt()
  const attempt: StoredLegacyWhatsAppReplyAttempt = {
    key: crypto.randomUUID(),
    randomSalt,
    bodySha256: await bodySha256(randomSalt, input.body),
    createdAt: now,
    serviceWindowEndsAt: canonicalServiceWindowEndsAt,
  }
  writeAttempt(storage, key, attempt)
  return { ...attempt, reused: false as const }
}

export function clearLegacyWhatsAppReplyAttempt(
  scope: AttemptScope,
  expectedKey?: string,
) {
  ensureNamespaceIsolation()
  removeAttempt(requireSessionStore(), storageKey(scope), expectedKey)
}

export function clearLegacyWhatsAppReplyAttemptNamespace() {
  namespaceIsolationVerified = false
  if (typeof window === 'undefined') return

  try {
    const storage = window.sessionStorage
    const matchingKeys: string[] = []
    for (let index = 0; index < storage.length; index += 1) {
      const key = storage.key(index)
      if (key?.startsWith(LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX)) {
        matchingKeys.push(key)
      }
    }
    for (const key of matchingKeys) verifyRemove(storage, key)

    // Verify the namespace is empty instead of trusting removeItem alone.
    for (let index = 0; index < storage.length; index += 1) {
      if (storage.key(index)?.startsWith(LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX)) {
        return
      }
    }
    namespaceIsolationVerified = true
  } catch {
    // Logout and organization switching must remain usable. The false flag
    // blocks subsequent direct replies until a cleanup can be verified.
  }
}
