import {
  LayoutDashboard,
  MessageSquare,
  Bot,
  FileText,
  Megaphone,
  Settings,
  Users,
  Contact,
  Workflow,
  Sparkles,
  Key,
  UserX,
  MessageSquareText,
  Webhook,
  BarChart3,
  ShieldCheck,
  Zap,
  Shield,
  LineChart,
  Tags,
  PhoneCall,
  PhoneForwarded,
  ScrollText,
  Building2,
  Rocket,
  Inbox,
  ListTodo,
  CalendarDays,
  Package,
  LockKeyhole,
  LifeBuoy
} from 'lucide-vue-next'
import type { Component } from 'vue'

export interface NavItem {
  name: string
  path: string
  icon: Component
  permission?: string
  requiredPermissions?: string[]
  anyPermissions?: string[]
  childPermissions?: string[]
  entitlement?: string
  exact?: boolean
  children?: NavItem[]
}

export interface NavSection {
  label: string
  items: NavItem[]
  /** Permissions needed to show section — at least one must pass */
  permissions: string[]
  /** Pin to bottom of sidebar */
  pinBottom?: boolean
}

export const navigationSections: NavSection[] = [
  {
    label: 'nav.sectionPartner',
    permissions: ['resellers'],
    items: [
      {
        name: 'nav.resellerConsole',
        path: '/resellers',
        icon: Building2,
        permission: 'resellers'
      }
    ]
  },
  {
    label: 'nav.sectionMain',
    permissions: ['onboarding', 'analytics', 'conversations', 'chat', 'contacts', 'tags', 'teams'],
    items: [
      {
        name: 'nav.launchpad',
        path: '/launchpad',
        icon: Rocket,
        permission: 'onboarding'
      },
      {
        name: 'nav.dashboard',
        path: '/',
        icon: LayoutDashboard,
        permission: 'analytics'
      },
      {
        name: 'nav.omnichannel',
        path: '/inbox',
        icon: Inbox,
        permission: 'conversations',
        requiredPermissions: ['conversations', 'channel_accounts'],
        entitlement: 'omnichannel.enabled'
      },
      {
        name: 'nav.chat',
        path: '/chat',
        icon: MessageSquare,
        permission: 'chat'
      },
      {
        name: 'nav.contacts',
        path: '/settings/contacts',
        icon: Contact,
        permission: 'contacts'
      },
      {
        name: 'nav.tags',
        path: '/settings/tags',
        icon: Tags,
        permission: 'tags'
      },
      {
        name: 'nav.teams',
        path: '/settings/teams',
        icon: Users,
        permission: 'teams'
      },
    ]
  },
  {
    label: 'nav.sectionGrowth',
    permissions: ['crm.leads', 'crm.automations', 'tasks', 'bookings', 'packages', 'payments', 'copilot'],
    items: [
      {
        name: 'nav.pipeline',
        path: '/crm/pipeline',
        icon: Workflow,
        permission: 'crm.leads',
        requiredPermissions: ['crm.leads', 'crm.pipelines'],
        entitlement: 'crm.enabled'
      },
      {
        name: 'nav.tasks',
        path: '/crm/tasks',
        icon: ListTodo,
        permission: 'tasks',
        entitlement: 'crm.enabled'
      },
      {
        name: 'nav.crmInsights',
        path: '/crm/insights',
        icon: LineChart,
        anyPermissions: ['crm.leads', 'tasks', 'bookings', 'packages', 'payments'],
        entitlement: 'crm.enabled'
      },
      {
        name: 'nav.automations',
        path: '/crm/automations',
        icon: Zap,
        permission: 'crm.automations',
        entitlement: 'crm.enabled'
      },
      {
        name: 'nav.calendar',
        path: '/calendar',
        icon: CalendarDays,
        permission: 'bookings',
        requiredPermissions: ['bookings', 'booking.settings'],
        entitlement: 'bookings.enabled'
      },
      {
        name: 'nav.commerce',
        path: '/commerce',
        icon: Package,
        permission: 'packages',
        childPermissions: ['packages', 'payments'],
        entitlement: 'commerce.enabled'
      },
      {
        name: 'nav.copilot',
        path: '/copilot',
        icon: Sparkles,
        permission: 'copilot',
        entitlement: 'copilot.enabled'
      }
    ]
  },
  {
    label: 'nav.sectionMessaging',
    permissions: ['accounts', 'settings.chatbot', 'chatbot.keywords', 'flows.chatbot', 'chatbot.ai', 'transfers', 'campaigns', 'templates', 'flows.whatsapp', 'canned_responses'],
    items: [
      {
        name: 'nav.accounts',
        path: '/settings/accounts',
        icon: Users,
        permission: 'accounts'
      },
      {
        name: 'nav.chatbot',
        path: '/chatbot',
        icon: Bot,
        permission: 'settings.chatbot',
        childPermissions: ['settings.chatbot', 'chatbot.keywords', 'flows.chatbot', 'chatbot.ai', 'transfers'],
        children: [
          { name: 'nav.overview', path: '/chatbot', icon: Bot, permission: 'settings.chatbot' },
          { name: 'nav.keywords', path: '/chatbot/keywords', icon: Key, permission: 'chatbot.keywords' },
          { name: 'nav.flows', path: '/chatbot/flows', icon: Workflow, permission: 'flows.chatbot' },
          { name: 'nav.aiContexts', path: '/chatbot/ai', icon: Sparkles, permission: 'chatbot.ai' },
          { name: 'nav.transfers', path: '/chatbot/transfers', icon: UserX, permission: 'transfers' }
        ]
      },
      {
        name: 'nav.campaigns',
        path: '/campaigns',
        icon: Megaphone,
        permission: 'campaigns'
      },
      {
        name: 'nav.templates',
        path: '/templates',
        icon: FileText,
        permission: 'templates'
      },
      {
        name: 'nav.cannedResponses',
        path: '/settings/canned-responses',
        icon: MessageSquareText,
        permission: 'canned_responses'
      },
      {
        name: 'nav.flows',
        path: '/flows',
        icon: Workflow,
        permission: 'flows.whatsapp'
      },
    ]
  },
  {
    label: 'nav.sectionCalling',
    permissions: ['call_logs', 'ivr_flows', 'call_transfers'],
    items: [
      { name: 'nav.callLogs', path: '/calling/logs', icon: PhoneCall, permission: 'call_logs' },
      { name: 'nav.ivrFlows', path: '/calling/ivr-flows', icon: Workflow, permission: 'ivr_flows' },
      { name: 'nav.callTransfers', path: '/calling/transfers', icon: PhoneForwarded, permission: 'call_transfers' },
    ]
  },
  {
    label: 'nav.sectionAnalytics',
    permissions: ['analytics.agents', 'analytics'],
    items: [
      {
        name: 'nav.agentAnalytics',
        path: '/analytics/agents',
        icon: BarChart3,
        permission: 'analytics.agents'
      },
      {
        name: 'nav.metaInsights',
        path: '/analytics/meta-insights',
        icon: LineChart,
        permission: 'analytics'
      },
    ]
  },
  {
    label: '',
    permissions: ['settings.general', 'users', 'roles', 'api_keys', 'webhooks', 'custom_actions', 'settings.sso', 'audit_logs', 'privacy.settings', 'privacy.requests', 'support'],
    pinBottom: true,
    items: [
      {
        name: 'nav.settings',
        path: '/settings',
        icon: Settings,
        permission: 'settings.general',
        childPermissions: ['settings.general', 'users', 'roles', 'api_keys', 'webhooks', 'custom_actions', 'settings.sso', 'audit_logs', 'privacy.settings', 'privacy.requests', 'support'],
        exact: true,
        children: [
          { name: 'nav.general', path: '/settings', icon: Settings, permission: 'settings.general', exact: true },
          { name: 'nav.users', path: '/settings/users', icon: Users, permission: 'users' },
          { name: 'nav.roles', path: '/settings/roles', icon: Shield, permission: 'roles' },
          { name: 'nav.apiKeys', path: '/settings/api-keys', icon: Key, permission: 'api_keys' },
          { name: 'nav.webhooks', path: '/settings/webhooks', icon: Webhook, permission: 'webhooks' },
          { name: 'nav.customActions', path: '/settings/custom-actions', icon: Zap, permission: 'custom_actions' },
          { name: 'nav.sso', path: '/settings/sso', icon: ShieldCheck, permission: 'settings.sso' },
          { name: 'nav.auditLogs', path: '/settings/audit-logs', icon: ScrollText, permission: 'audit_logs' },
          {
            name: 'nav.privacy',
            path: '/settings/privacy',
            icon: LockKeyhole,
            anyPermissions: ['privacy.settings', 'privacy.requests']
          },
          { name: 'nav.support', path: '/settings/support', icon: LifeBuoy, permission: 'support' }
        ]
      }
    ]
  }
]

// Flat list for backward compatibility (used by AppLayout computed)
export const navigationItems: NavItem[] = navigationSections.flatMap(s => s.items)
