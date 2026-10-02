/**
 * How to show a customer's address (phone number or channel identity) to
 * front-desk staff. WhatsApp coexistence contacts may only have a username
 * placeholder instead of a phone number.
 */

export interface ContactAddressInput {
  phone_number?: string | null
  whatsapp_account?: string | null
  metadata?: Record<string, unknown> | null
}

export interface ContactAddressDisplay {
  kind: 'phone' | 'whatsapp_hidden' | 'channel_id' | 'none'
  text: string
  hint: string
}

const PLACEHOLDER_PREFIXES = ['bsuid:', 'user:', 'event:', 'id:']

const CHANNEL_LABELS: Record<string, string> = {
  whatsapp: 'WhatsApp',
  instagram: 'Instagram',
  messenger: 'Messenger',
  facebook: 'Facebook',
  threads: 'Threads',
  email: 'Email',
  webchat: 'Website chat',
  tiktok: 'TikTok',
}

function channelLabel(channel: string) {
  return CHANNEL_LABELS[channel] ?? channel.charAt(0).toUpperCase() + channel.slice(1)
}

export function isPlaceholderPhone(phone?: string | null): boolean {
  const value = (phone ?? '').trim().toLowerCase()
  return PLACEHOLDER_PREFIXES.some((prefix) => value.startsWith(prefix))
}

export function contactAddressDisplay(
  contact: ContactAddressInput | null | undefined,
  opts?: { channel?: string | null },
): ContactAddressDisplay {
  if (!contact) return { kind: 'none', text: 'No phone number', hint: '' }
  const phone = (contact.phone_number ?? '').trim()
  const metadata = contact.metadata ?? {}
  const flaggedUnavailable =
    metadata.coexistence_phone_placeholder === true || metadata.coexistence_phone_unavailable === true
  // The coexistence flags can be stale once a real number arrives, so they only
  // hide a number that is itself unusable (masked, e.g. "6012****789").
  const masked = phone.includes('*')

  if (phone && !isPlaceholderPhone(phone) && !(flaggedUnavailable && masked)) {
    return { kind: 'phone', text: phone, hint: '' }
  }

  const channel = (opts?.channel ?? '').trim().toLowerCase()
  const hasWhatsAppAccount = Boolean((contact.whatsapp_account ?? '').trim())

  if (hasWhatsAppAccount || !channel || channel === 'whatsapp') {
    const username = metadata.coexistence_username
    const suffix = typeof username === 'string' && username.trim() ? ` (@${username.trim()})` : ''
    return {
      kind: 'whatsapp_hidden',
      text: `WhatsApp number hidden${suffix}`,
      hint: "WhatsApp did not share this customer's number (they message with a WhatsApp username).",
    }
  }

  const label = channelLabel(channel)
  return {
    kind: 'channel_id',
    text: `${label} customer`,
    hint: `This customer contacted you on ${label}; no phone number is linked.`,
  }
}

export interface ContactDisplayNameInput extends ContactAddressInput {
  name?: string | null
  profile_name?: string | null
}

function cleanName(value?: string | null): string {
  const text = (value ?? '').trim()
  return text && !isPlaceholderPhone(text) ? text : ''
}

/**
 * The name to show staff for a customer: their profile name, else the saved
 * name, else their phone number, else a plain description of the channel.
 * Never returns a raw placeholder such as "bsuid:...".
 */
export function contactDisplayName(
  contact: ContactDisplayNameInput | null | undefined,
  opts?: { channel?: string | null },
): string {
  if (!contact) return 'Customer'
  const named = cleanName(contact.profile_name) || cleanName(contact.name)
  if (named) return named
  const address = contactAddressDisplay(contact, opts)
  if (address.kind === 'phone') return address.text
  if (address.kind === 'whatsapp_hidden') return 'WhatsApp user'
  if (address.kind === 'channel_id') return address.text
  return 'Customer'
}
