import type {
  AutomationNodeType,
  AutomationPolicyGraph,
} from '@/services/automationPolicies'
import {
  CalendarCheck2,
  CalendarX2,
  PackageSearch,
  Receipt,
  type Icon,
} from 'lucide-vue-next'

export type AutomationTemplateKey =
  | 'post-visit'
  | 'no-show'
  | 'package-care'
  | 'overdue-invoice'

export interface AutomationPolicyTemplate {
  key: AutomationTemplateKey
  name: string
  shortName: string
  description: string
  eventLabel: string
  tone: 'cyan' | 'amber' | 'emerald' | 'rose'
  icon: Icon
  graph: AutomationPolicyGraph
}

const position = (x: number, y: number) => ({ x, y })

function node(
  id: string,
  type: AutomationNodeType,
  x: number,
  y: number,
  config: Record<string, unknown>,
) {
  return { id, type, position: position(x, y), config }
}

export const automationTemplates: AutomationPolicyTemplate[] = [
  {
    key: 'post-visit',
    name: 'Post-visit care',
    shortName: 'Post-visit',
    description: 'Create a thoughtful next-day follow-up task after a completed visit.',
    eventLabel: 'Booking completed',
    tone: 'cyan',
    icon: CalendarCheck2,
    graph: {
      nodes: [
        node('trigger_booking_status', 'trigger', 80, 160, {
          event_type: 'booking.status_changed',
        }),
        node('condition_completed', 'condition', 380, 160, {
          field: 'event.metadata.to_status',
          operator: 'equals',
          value: 'completed',
        }),
        node('delay_next_day', 'delay', 680, 100, { minutes: 1440 }),
        node('task_post_visit', 'create_task', 980, 100, {
          title: 'Check in after completed visit',
          description:
            'Review the visit notes and contact the customer with the approved post-visit care process.',
          priority: 'normal',
          due_in_minutes: 240,
          owner: 'contact_owner',
        }),
      ],
      edges: [
        { source: 'trigger_booking_status', target: 'condition_completed', branch: 'always' },
        { source: 'condition_completed', target: 'delay_next_day', branch: 'true' },
        { source: 'delay_next_day', target: 'task_post_visit', branch: 'always' },
      ],
    },
  },
  {
    key: 'no-show',
    name: 'No-show recovery',
    shortName: 'No-show',
    description: 'Put a recovery call into the queue one hour after a missed appointment.',
    eventLabel: 'Booking marked no-show',
    tone: 'amber',
    icon: CalendarX2,
    graph: {
      nodes: [
        node('trigger_booking_status', 'trigger', 80, 160, {
          event_type: 'booking.status_changed',
        }),
        node('condition_no_show', 'condition', 380, 160, {
          field: 'event.metadata.to_status',
          operator: 'equals',
          value: 'no_show',
        }),
        node('delay_one_hour', 'delay', 680, 100, { minutes: 60 }),
        node('task_recovery', 'create_task', 980, 100, {
          title: 'Recover missed appointment',
          description:
            'Review the booking context and offer an appropriate rebooking path.',
          priority: 'high',
          due_in_minutes: 120,
          owner: 'contact_owner',
        }),
      ],
      edges: [
        { source: 'trigger_booking_status', target: 'condition_no_show', branch: 'always' },
        { source: 'condition_no_show', target: 'delay_one_hour', branch: 'true' },
        { source: 'delay_one_hour', target: 'task_recovery', branch: 'always' },
      ],
    },
  },
  {
    key: 'package-care',
    name: 'Package expiring / low',
    shortName: 'Package care',
    description:
      'Surface a renewal review when finite package credits are running low.',
    eventLabel: 'Package balance low',
    tone: 'emerald',
    icon: PackageSearch,
    graph: {
      nodes: [
        node('trigger_package_low', 'trigger', 150, 160, {
          event_types: ['package.balance_low', 'package.expiring'],
        }),
        node('delay_next_morning', 'delay', 470, 160, { minutes: 720 }),
        node('task_package_care', 'create_task', 790, 160, {
          title: 'Review package continuity',
          description:
            'Check remaining credits, expiry context and the customer care plan before discussing renewal.',
          priority: 'normal',
          due_in_minutes: 1440,
          owner: 'contact_owner',
        }),
      ],
      edges: [
        { source: 'trigger_package_low', target: 'delay_next_morning', branch: 'always' },
        { source: 'delay_next_morning', target: 'task_package_care', branch: 'always' },
      ],
    },
  },
  {
    key: 'overdue-invoice',
    name: 'Overdue invoice',
    shortName: 'Invoice overdue',
    description: 'Create a controlled collections review when an invoice becomes overdue.',
    eventLabel: 'Invoice overdue',
    tone: 'rose',
    icon: Receipt,
    graph: {
      nodes: [
        node('trigger_invoice_overdue', 'trigger', 150, 160, {
          event_type: 'invoice.overdue',
        }),
        node('delay_review_window', 'delay', 470, 160, { minutes: 60 }),
        node('task_invoice_review', 'create_task', 790, 160, {
          title: 'Review overdue invoice',
          description:
            'Confirm the balance, payment history and customer context before taking an approved collections action.',
          priority: 'high',
          due_in_minutes: 240,
          owner: 'contact_owner',
        }),
      ],
      edges: [
        { source: 'trigger_invoice_overdue', target: 'delay_review_window', branch: 'always' },
        { source: 'delay_review_window', target: 'task_invoice_review', branch: 'always' },
      ],
    },
  },
]

export function cloneAutomationGraph(graph: AutomationPolicyGraph): AutomationPolicyGraph {
  return JSON.parse(JSON.stringify(graph)) as AutomationPolicyGraph
}

export function getAutomationTemplate(key: unknown) {
  return automationTemplates.find((template) => template.key === key)
}
