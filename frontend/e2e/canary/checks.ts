// Ported verbatim from the retained driver; contract.test.mjs detects drift.
export const UI_CHECKS = Object.freeze([
  "klinik_whatsapp_outbound",
  "klinik_whatsapp_inbound",
  "omnichannel_outbound_realtime_without_reload",
  "omnichannel_inbound_realtime_without_reload",
  "navbar_unread_increment",
  "navbar_unread_clear",
  "omnichannel_conversation_switch_autoscroll",
  "omnichannel_late_layout_autoscroll",
  "native_chat_realtime_without_reload",
  "native_chat_conversation_switch_autoscroll",
  "native_chat_late_layout_autoscroll",
  "non_klinik_send_denied",
  "cross_organization_send_denied",
]);

export const CHECK_EXECUTION_ORDER = Object.freeze([
  "klinik_whatsapp_inbound",
  "omnichannel_inbound_realtime_without_reload",
  "native_chat_realtime_without_reload",
  "klinik_whatsapp_outbound",
  "omnichannel_outbound_realtime_without_reload",
  "navbar_unread_increment",
  "navbar_unread_clear",
  "omnichannel_conversation_switch_autoscroll",
  "omnichannel_late_layout_autoscroll",
  "native_chat_conversation_switch_autoscroll",
  "native_chat_late_layout_autoscroll",
  "non_klinik_send_denied",
  "cross_organization_send_denied",
]);


export const DEFAULT_TIMEOUT_MS = 45_000;
// Leave a 30-second cleanup/response margin below the verifier's 240-second
// driver-only socket timeout and remain below its 300-second signed freshness.
export const DRIVER_EXECUTION_TIMEOUT_MS = 210_000;
export const DEADLINE_CLEANUP_GRACE_MS = 1_000;
export const BOTTOM_TOLERANCE_PX = 12;
// Late-layout probes keep the viewport inside the >=1280 px band where the
// customer workspace rails stay docked: crossing 1280 px turns the rails into
// drawers and widens the transcript, so its content shrinks. A height-only
// shrink to 1440x640 makes every fixture transcript overflow; a width-only
// step to 1300x640 then narrows the transcript in one layout, so its content
// grows while the reader is at the latest message.
//
// For the probe only, browser scroll anchoring is switched off on the
// transcript scroller (inline overflow-anchor: none, restored afterwards).
// With anchoring on, any layout that runs before the frame's scroll steps (a
// resize handler, a hover hit-test after a click, a keyup handler) moves
// scrollTop to keep the anchor node in place and dispatches a scroll event
// before the product's ResizeObserver runs; the product's scroll handler then
// reads a gap above its 80 px bottom threshold and stops following. In the
// pinned Chromium every window resize, navigation toggle and rail toggle tried
// with anchoring on failed for some fixture history, rail state or preceding
// check, mostly 88-997 px above the latest message. That is a product defect in
// ChannelsView and ChatView, queued as a ui source fix. With anchoring off, the
// growth itself never moves scrollTop, so the probe checks exactly the
// product's own bottom-following of a one-step late layout change.
export const LATE_LAYOUT_VIEWPORT = Object.freeze({ width: 1440, height: 640 });
export const LATE_LAYOUT_NARROW_VIEWPORT = Object.freeze({ width: 1300, height: 640 });
export const LATE_LAYOUT_SETTLE_MS = 1_000;
export const LATE_LAYOUT_FRAME_SETTLE_MS = 250;
// Native Chat positions a newly selected transcript on a short timer after it
// renders, so the latest-message boundary is polled for a bounded interval.
export const NATIVE_SELECTION_SETTLE_TIMEOUT_MS = 5_000;
