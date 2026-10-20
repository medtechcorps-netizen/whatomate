// A chat reads as unread when it has unread customer messages or a teammate
// marked it unread by hand (the list shows a dot when there are no unread
// messages). Read state is shared by the whole team.
export function contactHasUnread(
  contact: { unread_count?: number; manual_unread?: boolean } | null | undefined,
): boolean {
  if (!contact) return false
  return (contact.unread_count ?? 0) > 0 || contact.manual_unread === true
}
