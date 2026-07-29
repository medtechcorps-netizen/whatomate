<script setup lang="ts">
import { computed, markRaw, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  MarkerType,
  useVueFlow,
  type Connection,
  type EdgeMouseEvent,
  type NodeMouseEvent,
} from '@vue-flow/core'
import {
  Activity,
  AlertTriangle,
  ArrowLeft,
  BellRing,
  Check,
  CheckCircle2,
  CheckSquare2,
  ChevronRight,
  CircleDot,
  Clock3,
  Eye,
  FileClock,
  GitBranch,
  History,
  Layers3,
  Loader2,
  LockKeyhole,
  MessageSquareText,
  MoreHorizontal,
  Pause,
  Play,
  RefreshCw,
  Save,
  Settings2,
  ShieldCheck,
  Sparkles,
  Trash2,
  Workflow,
  XCircle,
} from 'lucide-vue-next'
import FlowCanvas from '@/components/shared/FlowCanvas.vue'
import UnsavedChangesDialog from '@/components/shared/UnsavedChangesDialog.vue'
import ConfirmDialog from '@/components/shared/ConfirmDialog.vue'
import AutomationPolicyNode from '@/components/automation/AutomationPolicyNode.vue'
import AutomationNodeInspector from '@/components/automation/AutomationNodeInspector.vue'
import AutomationPreviewDialog from '@/components/automation/AutomationPreviewDialog.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { useAppToast } from '@/composables/useAppToast'
import { useUnsavedChangesGuard } from '@/composables/useUnsavedChangesGuard'
import { getErrorMessage } from '@/lib/api-utils'
import {
  cloneAutomationGraph,
  getAutomationTemplate,
} from '@/lib/automationTemplates'
import {
  automationPolicyService,
  type AutomationEdgeBranch,
  type AutomationExecution,
  type AutomationNodeType,
  type AutomationPolicy,
  type AutomationPolicyCatalog,
  type AutomationPolicyGraph,
  type AutomationPolicyNode as PolicyNode,
  type AutomationPolicyVersion,
  type AutomationPreviewResult,
  type AutomationPreviewSimulationInput,
} from '@/services/automationPolicies'
import { useAuthStore } from '@/stores/auth'

interface LocalValidationIssue {
  message: string
  nodeId?: string
}

type StudioTab = 'builder' | 'runs' | 'versions' | 'safety'

const route = useRoute()
const router = useRouter()
const toast = useAppToast()
const authStore = useAuthStore()
const routePolicyId = computed(() => String(route.params.id ?? ''))
const isNew = computed(() => !routePolicyId.value || route.path.endsWith('/new'))

const catalog = ref<AutomationPolicyCatalog | null>(null)
const policy = ref<AutomationPolicy | null>(null)
const name = ref('')
const description = ref('')
const loading = ref(true)
const saving = ref(false)
const mutating = ref(false)
const previewing = ref(false)
const initialized = ref(false)
const hasChanges = ref(false)
const loadError = ref('')
const activeTab = ref<StudioTab>('builder')
const selectedNodeId = ref<string | null>(null)
const selectedEdgeId = ref<string | null>(null)
const showMobileInspector = ref(false)
const showPreview = ref(false)
const showActivateConfirm = ref(false)
const showDeleteConfirm = ref(false)
const previewResult = ref<AutomationPreviewResult | null>(null)
const previewFingerprint = ref('')
const versions = ref<AutomationPolicyVersion[]>([])
const executions = ref<AutomationExecution[]>([])
const versionsLoading = ref(false)
const executionsLoading = ref(false)
const runHistoryDenied = ref(false)

const canWrite = computed(
  () =>
    authStore.hasPermission('crm.automations', 'write') &&
    policy.value?.status !== 'archived',
)
const canExecute = computed(
  () =>
    authStore.hasPermission('crm.automations', 'execute') &&
    policy.value?.status !== 'archived',
)
const canActivate = computed(
  () => canExecute.value && authStore.hasPermission('tasks', 'write'),
)
const canReadRunHistory = computed(
  () =>
    authStore.hasPermission('contacts', 'read') &&
    authStore.hasPermission('tasks', 'read'),
)
const runHistoryUnavailable = computed(
  () => !canReadRunHistory.value || runHistoryDenied.value,
)
const canDelete = computed(() => authStore.hasPermission('crm.automations', 'delete'))
const readOnly = computed(() => !canWrite.value || policy.value?.status === 'archived')

const nodeTypes: Record<AutomationNodeType, any> = {
  trigger: markRaw(AutomationPolicyNode),
  condition: markRaw(AutomationPolicyNode),
  delay: markRaw(AutomationPolicyNode),
  create_task: markRaw(AutomationPolicyNode),
}

const {
  nodes,
  edges,
  addNodes,
  addEdges,
  removeNodes,
  removeEdges,
  setNodes,
  setEdges,
  onConnect,
  fitView,
} = useVueFlow({
  defaultEdgeOptions: {
    type: 'smoothstep',
    animated: false,
    markerEnd: MarkerType.ArrowClosed,
  },
})

const selectedNode = computed<PolicyNode | null>(() => {
  const node = nodes.value.find((item) => item.id === selectedNodeId.value)
  if (!node) return null
  return {
    id: node.id,
    type: node.type as AutomationNodeType,
    position: { x: node.position.x, y: node.position.y },
    config: { ...(node.data?.config ?? {}) },
  }
})

const triggerEventTypes = computed(() => {
  const trigger = nodes.value.find((node) => node.type === 'trigger')
  const config = trigger?.data?.config ?? {}
  if (Array.isArray(config.event_types)) return config.event_types.map(String)
  return config.event_type ? [String(config.event_type)] : []
})

const suggestedPreviewMetadata = computed<Record<string, unknown>>(() => {
  const condition = nodes.value.find((node) => node.type === 'condition')
  const config = condition?.data?.config ?? {}
  const field = String(config.field ?? '')
  if (
    String(config.operator ?? '') === 'equals' &&
    field.startsWith('event.metadata.') &&
    config.value !== undefined
  ) {
    return { [field.slice('event.metadata.'.length)]: config.value }
  }
  if (triggerEventTypes.value.includes('booking.status_changed')) {
    return { to_status: 'completed' }
  }
  return {}
})

const usesMarketingOptOutCondition = computed(() =>
  nodes.value.some(
    (node) =>
      node.type === 'condition' &&
      String(node.data?.config?.field ?? '') === 'contact.marketing_opt_out',
  ),
)

const hasTrigger = computed(() => nodes.value.some((node) => node.type === 'trigger'))
const activeVersionNumber = computed(() => policy.value?.active_version_number ?? 0)
const hasPublishedVersion = computed(() => activeVersionNumber.value > 0)
const isActive = computed(() => policy.value?.status === 'active')
const previewMatchesSavedDraft = computed(
  () =>
    Boolean(previewResult.value?.valid) &&
    !hasChanges.value &&
    previewFingerprint.value === graphFingerprint() &&
    previewResult.value?.version === policy.value?.version,
)
const previewIsCurrent = computed(
  () =>
    previewMatchesSavedDraft.value &&
    Boolean(previewResult.value?.actions?.length),
)
const previewWarningMessages = computed(() =>
  (previewResult.value?.warnings ?? []).map((issue) =>
    typeof issue === 'string' ? issue : issue.message,
  ),
)

const palette = [
  {
    type: 'trigger' as const,
    label: 'Customer event',
    description: 'Begin with a lifecycle event',
    icon: BellRing,
    tone: 'text-cyan-200 bg-cyan-300/10 light:text-cyan-800',
  },
  {
    type: 'condition' as const,
    label: 'Condition',
    description: 'Guard the path with event data',
    icon: GitBranch,
    tone: 'text-amber-200 bg-amber-300/10 light:text-amber-800',
  },
  {
    type: 'delay' as const,
    label: 'Wait',
    description: 'Resume safely after a delay',
    icon: Clock3,
    tone: 'text-violet-200 bg-violet-300/10 light:text-violet-800',
  },
  {
    type: 'create_task' as const,
    label: 'Create follow-up',
    description: 'Add accountable CRM work',
    icon: CheckSquare2,
    tone: 'text-emerald-200 bg-emerald-300/10 light:text-emerald-800',
  },
]

function newNodeId() {
  return `n_${crypto.randomUUID()}`
}

function instantiateGraph(graph: AutomationPolicyGraph): AutomationPolicyGraph {
  const source = cloneAutomationGraph(graph)
  const idMap = new Map(source.nodes.map((node) => [node.id, newNodeId()]))
  return {
    nodes: source.nodes.map((node) => ({
      ...node,
      id: idMap.get(node.id) ?? newNodeId(),
    })),
    edges: source.edges.map((edge) => ({
      ...edge,
      source: idMap.get(edge.source) ?? edge.source,
      target: idMap.get(edge.target) ?? edge.target,
    })),
  }
}

function blankGraph(): AutomationPolicyGraph {
  return {
    nodes: [
      {
        id: newNodeId(),
        type: 'trigger',
        position: { x: 120, y: 180 },
        config: { event_types: [] },
      },
    ],
    edges: [],
  }
}

function edgeStyle(branch?: AutomationEdgeBranch) {
  if (branch === 'true') return { stroke: '#86efac', strokeWidth: 2 }
  if (branch === 'false') return { stroke: '#64748b', strokeWidth: 2 }
  return { stroke: '#d9f99d', strokeWidth: 2 }
}

function fitCanvas() {
  if (window.innerWidth < 640 && nodes.value.length) {
    return fitView({
      nodes: [nodes.value[0].id],
      padding: 0.55,
      minZoom: 0.6,
      maxZoom: 0.82,
    })
  }
  return fitView({ padding: 0.22 })
}

function loadGraph(graph: AutomationPolicyGraph) {
  setNodes(
    (graph?.nodes ?? []).map((node) => ({
      id: node.id,
      type: node.type,
      position: node.position ?? { x: 100, y: 100 },
      data: { config: { ...(node.config ?? {}) } },
      selectable: true,
      focusable: true,
      deletable: false,
      draggable: !readOnly.value,
      ariaLabel: `${node.type.replace('_', ' ')} automation step`,
    })),
  )
  setEdges(
    (graph?.edges ?? []).map((edge, index) => {
      const branch = edge.branch ?? 'always'
      return {
        id: `e_${edge.source}_${branch}_${edge.target}_${index}`,
        source: edge.source,
        target: edge.target,
        sourceHandle: branch,
        type: 'smoothstep',
        animated: false,
        markerEnd: MarkerType.ArrowClosed,
        label: '',
        labelStyle: {
          fill: branch === 'true' ? '#86efac' : '#94a3b8',
          fontSize: 9,
          fontWeight: 700,
          letterSpacing: '0.08em',
        },
        labelBgStyle: { fill: '#0b0f10', fillOpacity: 0.88 },
        style: edgeStyle(branch),
        selectable: !readOnly.value,
      }
    }),
  )
  selectedNodeId.value = null
  selectedEdgeId.value = null
  nextTick(() => window.setTimeout(fitCanvas, 80))
}

function graphPayload(): AutomationPolicyGraph {
  return {
    nodes: nodes.value.map((node) => ({
      id: node.id,
      type: node.type as AutomationNodeType,
      position: {
        x: Math.round(node.position.x),
        y: Math.round(node.position.y),
      },
      config: JSON.parse(JSON.stringify(node.data?.config ?? {})),
    })),
    edges: edges.value.map((edge) => ({
      source: edge.source,
      target: edge.target,
      branch: (edge.sourceHandle || 'always') as AutomationEdgeBranch,
    })),
  }
}

function graphFingerprint() {
  return JSON.stringify(graphPayload())
}

function unwrapPolicy(response: any): AutomationPolicy {
  return (
    response?.data?.data?.automation_policy ??
    response?.data?.automation_policy ??
    response?.data?.data ??
    response?.data
  )
}

function unwrapVersions(response: any): AutomationPolicyVersion[] {
  return (
    response?.data?.data?.versions ??
    response?.data?.versions ??
    response?.data?.data ??
    []
  )
}

function unwrapExecutions(response: any): AutomationExecution[] {
  return (
    response?.data?.data?.executions ??
    response?.data?.executions ??
    response?.data?.data ??
    []
  )
}

async function loadCatalog() {
  try {
    const response = await automationPolicyService.catalog()
    catalog.value =
      response?.data?.data ??
      response?.data ??
      null
  } catch {
    // The curated labels remain a resilient fallback; policy validation stays server-authoritative.
    catalog.value = null
  }
}

async function loadPolicy() {
  loading.value = true
  loadError.value = ''
  initialized.value = false
  versions.value = []
  executions.value = []
  runHistoryDenied.value = false
  try {
    if (isNew.value) {
      const sourceId = String(route.query.source ?? '')
      const template = getAutomationTemplate(route.query.template)
      if (sourceId) {
        const response = await automationPolicyService.get(sourceId)
        const source = unwrapPolicy(response)
        name.value = `Copy of ${source.name}`
        description.value = source.description ?? ''
        loadGraph(instantiateGraph(source.graph))
      } else if (template) {
        name.value = template.name
        description.value = template.description
        loadGraph(instantiateGraph(template.graph))
      } else {
        name.value = 'Untitled automation policy'
        description.value = ''
        loadGraph(blankGraph())
      }
      policy.value = null
    } else {
      const response = await automationPolicyService.get(routePolicyId.value)
      policy.value = unwrapPolicy(response)
      name.value = policy.value.name
      description.value = policy.value.description ?? ''
      loadGraph(policy.value.graph)
    }
    hasChanges.value = isNew.value
    previewResult.value = null
    previewFingerprint.value = ''
    await nextTick()
    initialized.value = true
  } catch (error) {
    loadError.value = getErrorMessage(error, 'The automation policy could not be loaded.')
  } finally {
    loading.value = false
  }
}

function defaultConfig(type: AutomationNodeType): Record<string, unknown> {
  if (type === 'trigger') return { event_types: [] }
  if (type === 'condition') {
    return { field: 'event.metadata.to_status', operator: 'equals', value: '' }
  }
  if (type === 'delay') return { minutes: 60 }
  return {
    title: 'New customer follow-up',
    description: '',
    priority: 'normal',
    owner: 'unassigned',
  }
}

function addNode(type: AutomationNodeType) {
  if (readOnly.value) return
  if (type === 'trigger' && hasTrigger.value) {
    toast.info('One trigger can listen to multiple event types')
    const trigger = nodes.value.find((node) => node.type === 'trigger')
    if (trigger) selectNode(trigger.id)
    return
  }
  const index = nodes.value.length
  const id = newNodeId()
  addNodes([
    {
      id,
      type,
      position: {
        x: 180 + (index % 3) * 280,
        y: 120 + Math.floor(index / 3) * 180,
      },
      data: { config: defaultConfig(type) },
      selectable: true,
      focusable: true,
      deletable: false,
      draggable: true,
      ariaLabel: `${type.replace('_', ' ')} automation step`,
    },
  ])
  selectedNodeId.value = id
  showMobileInspector.value = true
  markChanged()
}

function selectNode(id: string) {
  selectedNodeId.value = id
  selectedEdgeId.value = null
  edges.value.forEach((edge) => {
    edge.selected = false
  })
  nodes.value.forEach((node) => {
    node.selected = node.id === id
  })
  if (window.innerWidth < 1280) showMobileInspector.value = true
}

function onNodeClick(event: NodeMouseEvent) {
  selectNode(event.node.id)
}

function onPaneClick() {
  selectedNodeId.value = null
  selectedEdgeId.value = null
  edges.value.forEach((edge) => {
    edge.selected = false
  })
}

function onEdgeClick(event: EdgeMouseEvent) {
  selectedNodeId.value = null
  selectedEdgeId.value = event.edge.id
  edges.value.forEach((edge) => {
    edge.selected = edge.id === event.edge.id
  })
}

function edgeWouldCreateCycle(source: string, target: string) {
  const adjacency = new Map<string, string[]>()
  for (const edge of edges.value) {
    const list = adjacency.get(edge.source) ?? []
    list.push(edge.target)
    adjacency.set(edge.source, list)
  }
  ;(adjacency.get(source) ?? []).push(target)
  if (!adjacency.has(source)) adjacency.set(source, [target])
  const stack = [target]
  const visited = new Set<string>()
  while (stack.length) {
    const current = stack.pop()!
    if (current === source) return true
    if (visited.has(current)) continue
    visited.add(current)
    stack.push(...(adjacency.get(current) ?? []))
  }
  return false
}

function handleConnect(connection: Connection) {
  if (readOnly.value || !connection.source || !connection.target) return
  if (connection.source === connection.target) {
    toast.warning('A step cannot connect to itself')
    return
  }
  const sourceNode = nodes.value.find((node) => node.id === connection.source)
  const targetNode = nodes.value.find((node) => node.id === connection.target)
  if (!sourceNode || !targetNode) return
  if (sourceNode.type === 'create_task') {
    toast.warning('A follow-up task is a terminal action')
    return
  }
  if (targetNode.type === 'trigger') {
    toast.warning('The customer event must remain the first step')
    return
  }
  if (edges.value.some((edge) => edge.target === connection.target)) {
    toast.warning('Each step can have only one previous step')
    return
  }
  if (edgeWouldCreateCycle(connection.source, connection.target)) {
    toast.warning('Automation policies must be acyclic')
    return
  }

  const branch =
    sourceNode.type === 'condition'
      ? ((connection.sourceHandle || 'true') as AutomationEdgeBranch)
      : 'always'
  const existing = edges.value.find(
    (edge) =>
      edge.source === connection.source &&
      (edge.sourceHandle || 'always') === branch,
  )
  if (existing) {
    toast.warning(
      sourceNode.type === 'condition'
        ? `The ${branch === 'true' ? 'match' : 'no-match'} path is already connected`
        : 'This step already has a next step',
    )
    return
  }

  addEdges([
    {
      id: `e_${connection.source}_${branch}_${connection.target}_${crypto.randomUUID()}`,
      source: connection.source,
      target: connection.target,
      sourceHandle: branch,
      targetHandle: connection.targetHandle,
      type: 'smoothstep',
      animated: false,
      markerEnd: MarkerType.ArrowClosed,
      label: '',
      labelStyle: {
        fill: branch === 'true' ? '#86efac' : '#94a3b8',
        fontSize: 9,
        fontWeight: 700,
      },
      labelBgStyle: { fill: '#0b0f10', fillOpacity: 0.88 },
      style: edgeStyle(branch),
    },
  ])
  markChanged()
}

onConnect(handleConnect)

function updateSelectedConfig(config: Record<string, unknown>) {
  const node = nodes.value.find((item) => item.id === selectedNodeId.value)
  if (!node || readOnly.value) return
  node.data = { ...node.data, config }
  markChanged()
}

function deleteSelectedNode() {
  const node = nodes.value.find((item) => item.id === selectedNodeId.value)
  if (!node || readOnly.value) return
  if (node.type === 'trigger') {
    toast.warning('Choose events on the existing trigger instead')
    return
  }
  removeEdges(
    edges.value.filter((edge) => edge.source === node.id || edge.target === node.id),
  )
  removeNodes([node.id])
  selectedNodeId.value = null
  showMobileInspector.value = false
  markChanged()
}

function deleteSelectedEdge() {
  if (!selectedEdgeId.value || readOnly.value) return
  removeEdges([selectedEdgeId.value])
  selectedEdgeId.value = null
  markChanged()
}

function onEdgesChange(changes: Array<{ type?: string; id?: string }>) {
  const removed = changes.filter((change) => change.type === 'remove')
  if (!removed.length) return
  if (removed.some((change) => change.id === selectedEdgeId.value)) {
    selectedEdgeId.value = null
  }
  markChanged()
}

function markChanged() {
  if (!initialized.value) return
  hasChanges.value = true
  previewResult.value = null
}

function localValidation(): LocalValidationIssue[] {
  const issues: LocalValidationIssue[] = []
  const graph = graphPayload()
  const ids = new Set(graph.nodes.map((node) => node.id))
  const incoming = new Map<string, AutomationPolicyGraph['edges']>()
  const outgoing = new Map<string, AutomationPolicyGraph['edges']>()
  for (const edge of graph.edges) {
    incoming.set(edge.target, [...(incoming.get(edge.target) ?? []), edge])
    outgoing.set(edge.source, [...(outgoing.get(edge.source) ?? []), edge])
  }
  const triggers = graph.nodes.filter((node) => node.type === 'trigger')
  if (triggers.length !== 1) {
    issues.push({ message: 'Add exactly one customer event trigger.', nodeId: triggers[0]?.id })
  } else {
    const config = triggers[0].config
    const events = Array.isArray(config.event_types)
      ? config.event_types
      : config.event_type
        ? [config.event_type]
        : []
    if (!events.length) {
      issues.push({ message: 'Choose at least one trigger event.', nodeId: triggers[0].id })
    }
  }
  if (!graph.nodes.some((node) => node.type === 'create_task')) {
    issues.push({ message: 'Add at least one reachable follow-up action.' })
  }
  for (const node of graph.nodes) {
    const nodeIncoming = incoming.get(node.id) ?? []
    const nodeOutgoing = outgoing.get(node.id) ?? []
    if (node.type === 'trigger' && nodeIncoming.length) {
      issues.push({ message: 'The customer event cannot have a previous step.', nodeId: node.id })
    }
    if (node.type !== 'trigger' && nodeIncoming.length !== 1) {
      issues.push({ message: 'Connect exactly one previous step.', nodeId: node.id })
    }
    if (
      (node.type === 'trigger' || node.type === 'delay') &&
      nodeOutgoing.length !== 1
    ) {
      issues.push({ message: 'Connect exactly one next step.', nodeId: node.id })
    }
    if (node.type === 'create_task' && nodeOutgoing.length) {
      issues.push({ message: 'A follow-up must remain the final step.', nodeId: node.id })
    }
    if (node.type === 'condition') {
      const branches = nodeOutgoing.map((edge) => edge.branch)
      if (
        !nodeOutgoing.length ||
        nodeOutgoing.length > 2 ||
        branches.some((branch) => branch !== 'true' && branch !== 'false') ||
        new Set(branches).size !== branches.length
      ) {
        issues.push({
          message: 'Connect one unique match or no-match branch.',
          nodeId: node.id,
        })
      }
    }
    if (node.type === 'condition') {
      const isBooleanCondition = node.config.field === 'contact.marketing_opt_out'
      if (!node.config.field || !node.config.operator) {
        issues.push({ message: 'Complete this condition.', nodeId: node.id })
      } else if (isBooleanCondition && node.config.operator === 'in') {
        issues.push({
          message: 'Use equals, does not equal, or exists for marketing opt-out.',
          nodeId: node.id,
        })
      } else if (
        isBooleanCondition &&
        node.config.operator !== 'exists' &&
        typeof node.config.value !== 'boolean'
      ) {
        issues.push({
          message: 'Choose whether the customer has opted out.',
          nodeId: node.id,
        })
      } else if (
        node.config.operator === 'in' &&
        (!Array.isArray(node.config.value) ||
          !node.config.value.length ||
          node.config.value.some((value) => typeof value !== 'string' || !value.trim()))
      ) {
        issues.push({ message: 'Add at least one allowed value to this condition.', nodeId: node.id })
      } else if (
        node.config.operator !== 'exists' &&
        node.config.operator !== 'in' &&
        (typeof node.config.value !== 'string' || !node.config.value.trim())
      ) {
        issues.push({ message: 'Add the expected value for this condition.', nodeId: node.id })
      }
    }
    if (node.type === 'delay') {
      const minutes = Number(node.config.minutes)
      if (!Number.isInteger(minutes) || minutes < 0 || minutes > 43200) {
        issues.push({ message: 'Delay must be between 0 and 43,200 minutes.', nodeId: node.id })
      }
    }
    if (node.type === 'create_task') {
      if (!String(node.config.title ?? '').trim()) {
        issues.push({ message: 'Give this follow-up a title.', nodeId: node.id })
      }
      const dueMinutes = node.config.due_in_minutes
      const remindMinutes = node.config.remind_in_minutes
      for (const value of [dueMinutes, remindMinutes]) {
        if (
          value !== undefined &&
          (typeof value !== 'number' ||
            !Number.isInteger(value) ||
            value < 0 ||
            value > 525600)
        ) {
          issues.push({
            message: 'Task schedules must be between 0 and 525,600 minutes.',
            nodeId: node.id,
          })
          break
        }
      }
      if (
        typeof dueMinutes === 'number' &&
        Number.isInteger(dueMinutes) &&
        typeof remindMinutes === 'number' &&
        Number.isInteger(remindMinutes) &&
        remindMinutes > dueMinutes
      ) {
        issues.push({
          message: 'The reminder cannot be after the task due time.',
          nodeId: node.id,
        })
      }
    }
  }
  for (const edge of graph.edges) {
    if (!ids.has(edge.source) || !ids.has(edge.target)) {
      issues.push({ message: 'Remove the connection to a missing step.' })
    }
  }
  if (triggers.length === 1) {
    const reachable = new Set<string>()
    const queue = [triggers[0].id]
    while (queue.length) {
      const current = queue.shift()!
      if (reachable.has(current)) continue
      reachable.add(current)
      for (const edge of outgoing.get(current) ?? []) queue.push(edge.target)
    }
    const unreachable = graph.nodes.find((node) => !reachable.has(node.id))
    if (unreachable) {
      issues.push({
        message: 'Connect this step to the customer event path.',
        nodeId: unreachable.id,
      })
    }
  }
  return issues
}

const validationIssues = computed(localValidation)

function focusIssue(issue: LocalValidationIssue) {
  if (issue.nodeId) {
    activeTab.value = 'builder'
    selectNode(issue.nodeId)
  }
}

async function saveDraft() {
  if (!canWrite.value) return false
  if (!name.value.trim()) {
    toast.warning('Give this policy a name')
    return false
  }
  saving.value = true
  try {
    const input = {
      name: name.value.trim(),
      description: description.value.trim(),
      graph: graphPayload(),
    }
    let response
    if (policy.value) {
      response = await automationPolicyService.update(policy.value.id, {
        ...input,
        version: policy.value.version,
      })
    } else {
      response = await automationPolicyService.create(input)
    }
    const updated = unwrapPolicy(response)
    policy.value = updated
    name.value = updated.name
    description.value = updated.description ?? ''
    hasChanges.value = false
    toast.success('Draft saved', `Revision ${updated.version} is ready to preview.`)
    if (isNew.value) {
      await router.replace(`/crm/automations/${updated.id}`)
    }
    return true
  } catch (error: any) {
    if (error?.response?.status === 409 && policy.value) {
      toast.warning('A newer draft exists', 'Reloading the latest revision before you continue.')
      await loadPolicy()
    } else {
      toast.error('Draft was not saved', getErrorMessage(error))
    }
    return false
  } finally {
    saving.value = false
  }
}

function openPreview() {
  if (!policy.value) {
    toast.info('Save the draft before previewing it')
    return
  }
  showPreview.value = true
}

async function runPreview(input: AutomationPreviewSimulationInput) {
  if (!policy.value || !canExecute.value) return
  previewing.value = true
  try {
    const fingerprint = graphFingerprint()
    const response = await automationPolicyService.preview(policy.value.id, {
      ...input,
      graph: graphPayload(),
      version: policy.value.version,
    })
    previewResult.value =
      response?.data?.data?.preview ??
      response?.data?.preview ??
      response?.data?.data ??
      response?.data
    previewFingerprint.value = fingerprint
  } catch (error: any) {
    if (error?.response?.status === 409) {
      toast.warning('The draft changed before preview', 'Reload the latest revision and try again.')
      await loadPolicy()
      showPreview.value = false
    } else {
      toast.error('Preview could not run', getErrorMessage(error))
    }
  } finally {
    previewing.value = false
  }
}

function requestActivate() {
  if (!policy.value || !canExecute.value) return
  if (!canActivate.value) {
    toast.warning('Task write permission is required to activate this policy')
    return
  }
  if (hasChanges.value) {
    toast.warning('Save the current draft before activation')
    return
  }
  if (validationIssues.value.length) {
    toast.warning('Resolve the policy checks before activation')
    focusIssue(validationIssues.value[0])
    return
  }
  if (previewMatchesSavedDraft.value && !previewResult.value?.actions?.length) {
    toast.warning(
      'Preview a path that reaches a follow-up',
      'Adjust the sample event, metadata, actor, source or contact opt-out value until a projected task appears.',
    )
    openPreview()
    return
  }
  if (!previewIsCurrent.value) {
    toast.warning('Preview this saved draft before activation')
    openPreview()
    return
  }
  showActivateConfirm.value = true
}

async function activate() {
  if (!policy.value || !canActivate.value) return
  mutating.value = true
  try {
    const hadPublishedVersion = hasPublishedVersion.value
    const response = await automationPolicyService.activate(
      policy.value.id,
      policy.value.version,
    )
    policy.value = unwrapPolicy(response)
    const activationWarnings = (policy.value.validation_warnings ?? []).map(
      (warning) => warning.message,
    )
    showActivateConfirm.value = false
    previewResult.value = null
    previewFingerprint.value = ''
    toast.success(
      hadPublishedVersion ? 'New version published' : 'Policy activated',
      `Version ${policy.value.active_version_number} is now immutable and live.`,
    )
    if (activationWarnings.length) {
      toast.warning(
        'Parallel workflow warning',
        activationWarnings.slice(0, 3).join(' '),
      )
    }
    await loadVersions()
  } catch (error: any) {
    if (error?.response?.status === 409) {
      toast.warning('Activation stopped', getErrorMessage(error))
      await loadPolicy()
    } else {
      toast.error('Policy was not activated', getErrorMessage(error))
    }
  } finally {
    mutating.value = false
  }
}

async function pausePolicy() {
  if (!policy.value || !canExecute.value) return
  mutating.value = true
  try {
    const response = await automationPolicyService.pause(
      policy.value.id,
      policy.value.version,
    )
    policy.value = unwrapPolicy(response)
    toast.success('Policy paused', 'No new customer events will start this policy.')
  } catch (error: any) {
    if (error?.response?.status === 409) {
      toast.warning('Policy changed', 'Reloading the current status.')
      await loadPolicy()
    } else {
      toast.error('Policy was not paused', getErrorMessage(error))
    }
  } finally {
    mutating.value = false
  }
}

async function deleteDraft() {
  if (!policy.value || !canDelete.value || policy.value.status !== 'draft') return
  mutating.value = true
  try {
    await automationPolicyService.remove(policy.value.id, policy.value.version)
    hasChanges.value = false
    toast.success('Draft policy deleted')
    await router.replace('/crm/automations')
  } catch (error) {
    toast.error('Draft was not deleted', getErrorMessage(error))
  } finally {
    mutating.value = false
  }
}

async function loadVersions() {
  if (!policy.value) return
  versionsLoading.value = true
  try {
    const versionResponse = await automationPolicyService.versions(policy.value.id)
    versions.value = unwrapVersions(versionResponse)
  } catch (error) {
    toast.error('Version history could not be loaded', getErrorMessage(error))
  } finally {
    versionsLoading.value = false
  }
}

async function loadExecutions() {
  if (!policy.value || !canReadRunHistory.value) {
    executions.value = []
    return
  }
  executionsLoading.value = true
  runHistoryDenied.value = false
  try {
    const executionResponse = await automationPolicyService.executions(
      policy.value.id,
      { limit: 50 },
    )
    executions.value = unwrapExecutions(executionResponse)
  } catch (error: any) {
    executions.value = []
    if (error?.response?.status === 403) {
      runHistoryDenied.value = true
    } else {
      toast.error('Run history could not be loaded', getErrorMessage(error))
    }
  } finally {
    executionsLoading.value = false
  }
}

function selectTab(tab: StudioTab) {
  activeTab.value = tab
  if (tab === 'versions' && policy.value) loadVersions()
  if (tab === 'runs' && policy.value && canReadRunHistory.value) loadExecutions()
}

function formatDate(value?: string) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('en-MY', {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  }).format(new Date(value))
}

function executionStatusClass(status: string) {
  if (status === 'completed') return 'text-emerald-300 bg-emerald-300/10 light:text-emerald-800'
  if (status === 'failed') return 'text-rose-300 bg-rose-300/10 light:text-rose-800'
  if (status === 'skipped' || status === 'cancelled') return 'text-slate-300 bg-slate-300/10 light:text-slate-700'
  return 'text-amber-300 bg-amber-300/10 light:text-amber-800'
}

function executionBranchLabel(branch?: AutomationEdgeBranch) {
  if (branch === 'true') return 'Match path'
  if (branch === 'false') return 'No-match path'
  if (branch === 'always') return 'Continue path'
  return ''
}

function statusVariant() {
  if (policy.value?.status === 'active') return 'success'
  if (policy.value?.status === 'paused') return 'warning'
  if (policy.value?.status === 'archived') return 'destructive'
  return 'secondary'
}

function handlePreviewNode(nodeId: string) {
  showPreview.value = false
  activeTab.value = 'builder'
  nextTick(() => selectNode(nodeId))
}

watch([name, description], () => markChanged())
watch(routePolicyId, (next, previous) => {
  if (next !== previous) loadPolicy()
})

let resizeTimer: number | undefined
function refitAfterResize() {
  window.clearTimeout(resizeTimer)
  resizeTimer = window.setTimeout(fitCanvas, 120)
}

const {
  showLeaveDialog,
  confirmLeave,
  cancelLeave,
} = useUnsavedChangesGuard(hasChanges)

onMounted(() => {
  window.addEventListener('resize', refitAfterResize)
  loadCatalog()
  loadPolicy()
})

onBeforeUnmount(() => {
  window.clearTimeout(resizeTimer)
  window.removeEventListener('resize', refitAfterResize)
})
</script>

<template>
  <div class="automation-studio flex h-full min-h-0 flex-col overflow-hidden bg-[#080b0c] light:bg-[#f5f6f3]">
    <header class="shrink-0 border-b border-white/[0.08] bg-[#0a0d0e]/95 backdrop-blur light:border-gray-200 light:bg-white/95">
      <div class="flex min-h-16 items-center gap-3 px-3 sm:px-5">
        <RouterLink to="/crm/automations">
          <Button variant="ghost" size="icon" aria-label="Back to automation policies">
            <ArrowLeft class="h-4 w-4" />
          </Button>
        </RouterLink>
        <span class="hidden h-8 w-px bg-white/[0.08] light:bg-gray-200 sm:block" />
        <span class="hidden h-8 w-8 shrink-0 items-center justify-center rounded-xl bg-gradient-to-br from-lime-300 to-emerald-600 text-[#11170c] shadow-lg shadow-lime-400/10 sm:flex">
          <Workflow class="h-4 w-4" />
        </span>

        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <input
              v-if="!readOnly && !loading"
              v-model.trim="name"
              maxlength="150"
              aria-label="Policy name"
              class="min-w-0 max-w-xl flex-1 truncate border-0 bg-transparent p-0 text-sm font-semibold text-white outline-none placeholder:text-white/25 focus:ring-0 light:text-gray-900"
              placeholder="Untitled automation policy"
            />
            <p v-else class="truncate text-sm font-semibold text-white light:text-gray-900">
              {{ loading ? 'Loading policy…' : name }}
            </p>
            <Badge v-if="policy" :variant="statusVariant()" class="hidden capitalize sm:inline-flex">
              {{ policy.status }}
            </Badge>
            <span v-if="hasChanges" class="hidden items-center gap-1 text-[10px] text-amber-300 md:flex">
              <CircleDot class="h-3 w-3" />
              Unsaved
            </span>
          </div>
          <input
            v-if="!readOnly && !loading"
            v-model.trim="description"
            maxlength="4000"
            aria-label="Policy description"
            class="mt-0.5 hidden w-full max-w-2xl truncate border-0 bg-transparent p-0 text-[10px] text-white/40 outline-none placeholder:text-white/20 focus:text-white/65 focus:ring-0 light:text-gray-600 light:focus:text-gray-900 sm:block"
            placeholder="Add a short purpose for this policy"
          />
          <p
            v-else-if="description"
            class="mt-0.5 hidden truncate text-[10px] text-white/40 light:text-gray-600 sm:block"
          >
            {{ description }}
          </p>
          <p class="mt-0.5 truncate text-[10px] text-white/30 light:text-gray-500">
            <template v-if="hasPublishedVersion">Live version {{ activeVersionNumber }} is immutable · </template>
            <template v-if="policy">Draft revision {{ policy.version }}</template>
            <template v-else>New draft</template>
          </p>
        </div>

        <div class="flex shrink-0 items-center gap-1.5 sm:gap-2">
          <Button
            v-if="canWrite"
            data-testid="automation-save-draft"
            variant="outline"
            size="sm"
            class="gap-2"
            :disabled="saving || loading || !hasChanges"
            @click="saveDraft"
          >
            <Loader2 v-if="saving" class="h-3.5 w-3.5 animate-spin" />
            <Save v-else class="h-3.5 w-3.5" />
            <span class="hidden md:inline">Save draft</span>
          </Button>
          <Button
            v-if="canExecute"
            data-testid="automation-preview"
            variant="outline"
            size="sm"
            class="gap-2 border-cyan-300/20 text-cyan-100 hover:bg-cyan-300/10 light:text-cyan-800"
            :disabled="loading || !policy"
            @click="openPreview"
          >
            <Eye class="h-3.5 w-3.5" />
            <span class="hidden md:inline">Preview</span>
          </Button>
          <Button
            v-if="canExecute"
            data-testid="automation-activate"
            size="sm"
            class="gap-2 bg-lime-300 text-[#11170c] hover:bg-lime-200"
            :disabled="loading || mutating || !policy || !canActivate"
            :title="canActivate ? undefined : 'Task write permission is required to activate policies'"
            :aria-describedby="canActivate ? undefined : 'automation-activate-permission'"
            @click="requestActivate"
          >
            <Play class="h-3.5 w-3.5" />
            <span class="hidden sm:inline">{{ hasPublishedVersion ? 'Publish version' : 'Activate' }}</span>
          </Button>
          <span
            v-if="canExecute && !canActivate"
            id="automation-activate-permission"
            class="sr-only"
          >
            Task write permission is required to activate policies.
          </span>
          <Button
            v-if="canExecute && isActive"
            variant="outline"
            size="sm"
            class="gap-2 border-amber-300/20 text-amber-200 hover:bg-amber-300/10 light:text-amber-800"
            :disabled="mutating"
            @click="pausePolicy"
          >
            <Pause class="h-3.5 w-3.5" />
            <span class="hidden sm:inline">Pause</span>
          </Button>
          <Button
            v-if="selectedNode"
            variant="ghost"
            size="icon"
            class="xl:hidden"
            aria-label="Open step settings"
            @click="showMobileInspector = true"
          >
            <Settings2 class="h-4 w-4" />
          </Button>
          <DropdownMenu v-if="policy">
            <DropdownMenuTrigger as-child>
              <Button variant="ghost" size="icon" aria-label="More policy actions">
                <MoreHorizontal class="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem @click="selectTab('versions')">
                <History class="mr-2 h-4 w-4" />
                Version history
              </DropdownMenuItem>
              <DropdownMenuItem @click="selectTab('runs')">
                <LockKeyhole v-if="runHistoryUnavailable" class="mr-2 h-4 w-4" />
                <Activity v-else class="mr-2 h-4 w-4" />
                Run monitor
              </DropdownMenuItem>
              <template v-if="canDelete && policy.status === 'draft'">
                <DropdownMenuSeparator />
                <DropdownMenuItem class="text-destructive focus:text-destructive" @click="showDeleteConfirm = true">
                  <Trash2 class="mr-2 h-4 w-4" />
                  Delete draft
                </DropdownMenuItem>
              </template>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      <div class="flex items-center justify-between gap-3 border-t border-white/[0.055] px-3 light:border-gray-100 sm:px-5">
        <nav class="flex min-w-0 overflow-x-auto" aria-label="Automation policy sections">
          <button
            v-for="tab in [
              { key: 'builder', label: 'Builder', icon: Layers3 },
              { key: 'runs', label: 'Runs', icon: Activity },
              { key: 'versions', label: 'Versions', icon: FileClock },
              { key: 'safety', label: 'Safety', icon: ShieldCheck },
            ]"
            :key="tab.key"
            type="button"
            class="relative flex h-11 shrink-0 cursor-pointer items-center gap-2 px-3 text-xs font-medium outline-none transition focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-lime-200"
            :class="activeTab === tab.key
              ? 'text-lime-200 light:text-lime-800'
              : 'text-white/35 hover:text-white/65 light:text-gray-500 light:hover:text-gray-900'"
            @click="selectTab(tab.key as StudioTab)"
          >
            <component :is="tab.icon" class="h-3.5 w-3.5" />
            {{ tab.label }}
            <LockKeyhole
              v-if="tab.key === 'runs' && runHistoryUnavailable"
              class="h-3 w-3 opacity-65"
              aria-label="Additional permissions required"
            />
            <span v-if="activeTab === tab.key" class="absolute inset-x-2 bottom-0 h-0.5 rounded-full bg-lime-200 light:bg-lime-700" />
          </button>
        </nav>
        <button
          v-if="validationIssues.length"
          type="button"
          class="hidden cursor-pointer items-center gap-1.5 rounded-full bg-amber-300/[0.08] px-2.5 py-1 text-[10px] text-amber-200 outline-none focus-visible:ring-2 focus-visible:ring-amber-300 sm:flex"
          @click="focusIssue(validationIssues[0])"
        >
          <AlertTriangle class="h-3 w-3" />
          {{ validationIssues.length }} {{ validationIssues.length === 1 ? 'check' : 'checks' }}
        </button>
        <span v-else class="hidden items-center gap-1.5 text-[10px] text-emerald-300 sm:flex">
          <CheckCircle2 class="h-3 w-3" />
          Graph checks passed
        </span>
      </div>
    </header>

    <div v-if="loading" class="flex min-h-0 flex-1 items-center justify-center">
      <div class="text-center">
        <Loader2 class="mx-auto h-7 w-7 animate-spin text-lime-200 light:text-lime-800" />
        <p class="mt-3 text-xs text-white/35 light:text-gray-600">Loading automation studio…</p>
      </div>
    </div>

    <div v-else-if="loadError" class="flex min-h-0 flex-1 items-center justify-center p-6">
      <div class="max-w-md text-center">
        <XCircle class="mx-auto h-8 w-8 text-rose-300" />
        <h2 class="mt-4 text-lg font-semibold text-white light:text-gray-900">Policy unavailable</h2>
        <p class="mt-2 text-sm text-white/40 light:text-gray-600">{{ loadError }}</p>
        <Button class="mt-5 gap-2" @click="loadPolicy">
          <RefreshCw class="h-4 w-4" />
          Try again
        </Button>
      </div>
    </div>

    <div v-else-if="activeTab === 'builder'" class="grid min-h-0 flex-1 grid-rows-[auto_minmax(0,1fr)] xl:grid-cols-[232px_minmax(0,1fr)_342px] xl:grid-rows-1">
      <aside data-testid="automation-palette" class="border-b border-white/[0.08] bg-[#0c0f10] p-3 light:border-gray-200 light:bg-white xl:border-b-0 xl:border-r xl:p-4" aria-label="Automation step palette">
        <div class="hidden xl:block">
          <p class="text-[9px] font-bold uppercase tracking-[0.22em] text-white/30 light:text-gray-500">Step palette</p>
          <h2 class="mt-1 text-sm font-semibold text-white light:text-gray-900">Build the policy</h2>
          <p class="mt-1 text-[11px] leading-4 text-white/30 light:text-gray-600">Add steps, then connect their handles.</p>
        </div>

        <div class="flex gap-2 overflow-x-auto xl:mt-4 xl:block xl:space-y-2">
          <button
            v-for="item in palette"
            :key="item.type"
            type="button"
            class="group flex min-w-[164px] cursor-pointer items-center gap-3 rounded-xl border border-white/[0.075] bg-white/[0.025] p-2.5 text-left outline-none transition hover:border-lime-200/20 hover:bg-white/[0.045] focus-visible:ring-2 focus-visible:ring-lime-200 disabled:cursor-not-allowed disabled:opacity-35 light:border-gray-200 light:bg-gray-50 xl:w-full"
            :disabled="readOnly || (item.type === 'trigger' && hasTrigger)"
            @click="addNode(item.type)"
          >
            <span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-xl" :class="item.tone">
              <component :is="item.icon" class="h-4 w-4" />
            </span>
            <span class="min-w-0">
              <span class="block text-xs font-medium text-white/75 light:text-gray-800">{{ item.label }}</span>
              <span class="mt-0.5 hidden text-[9px] leading-3 text-white/25 light:text-gray-500 xl:block">{{ item.description }}</span>
            </span>
          </button>
          <button
            type="button"
            disabled
            class="flex min-w-[164px] items-center gap-3 rounded-xl border border-dashed border-white/[0.07] p-2.5 text-left opacity-45 light:border-gray-300 xl:w-full"
          >
            <span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-xl bg-white/[0.04] text-white/30 light:bg-gray-100 light:text-gray-500">
              <MessageSquareText class="h-4 w-4" />
            </span>
            <span>
              <span class="flex items-center gap-1.5 text-xs font-medium text-white/45 light:text-gray-600">
                Send message
                <LockKeyhole class="h-3 w-3" />
              </span>
              <span class="mt-0.5 hidden text-[9px] leading-3 text-white/25 light:text-gray-500 xl:block">Future reviewed release</span>
            </span>
          </button>
        </div>

        <div class="mt-5 hidden rounded-2xl border border-emerald-300/10 bg-emerald-300/[0.04] p-3 xl:block">
          <p class="flex items-center gap-2 text-[10px] font-bold uppercase tracking-[0.15em] text-emerald-200 light:text-emerald-800">
            <ShieldCheck class="h-3.5 w-3.5" />
            Task-only mode
          </p>
          <p class="mt-1.5 text-[10px] leading-4 text-white/35 light:text-gray-600">
            No node in this release can contact a customer.
          </p>
        </div>
      </aside>

      <section data-testid="automation-canvas" class="relative min-h-0 overflow-hidden bg-[#090c0e] light:bg-[#f0f3ef]" aria-label="Automation policy canvas">
        <div class="absolute left-4 top-4 z-10 flex items-center gap-2 rounded-xl border border-white/[0.08] bg-[#0b0e10]/90 px-3 py-2 text-[10px] text-white/35 shadow-xl backdrop-blur light:border-gray-200 light:bg-white/90 light:text-gray-600">
          <Sparkles class="h-3.5 w-3.5 text-lime-200 light:text-lime-800" />
          {{ readOnly ? 'View-only policy graph' : 'Drag steps · connect left to right' }}
        </div>
        <Button
          v-if="selectedEdgeId && !readOnly"
          variant="outline"
          size="sm"
          class="absolute right-4 top-4 z-10 gap-2 border-rose-300/20 bg-[#0b0e10]/90 text-rose-200 shadow-xl backdrop-blur hover:bg-rose-300/10 light:bg-white/90 light:text-rose-700"
          @click="deleteSelectedEdge"
        >
          <Trash2 class="h-3.5 w-3.5" />
          Remove connection
        </Button>
        <FlowCanvas
          :nodes="nodes"
          :edges="edges"
          :node-types="nodeTypes"
          :nodes-draggable="!readOnly"
          :nodes-connectable="!readOnly"
          :edges-updatable="false"
          :delete-enabled="!readOnly"
          edge-type="smoothstep"
          controls-position="bottom-left"
          @node-click="onNodeClick"
          @pane-click="onPaneClick"
          @edge-click="onEdgeClick"
          @node-drag-stop="markChanged"
          @edges-change="onEdgesChange"
        />
      </section>

      <div class="hidden min-h-0 border-l border-white/[0.08] light:border-gray-200 xl:block">
        <AutomationNodeInspector
          :catalog="catalog"
          :node="selectedNode"
          :readonly="readOnly"
          @update="updateSelectedConfig"
          @delete="deleteSelectedNode"
        />
      </div>
    </div>

    <main v-else class="min-h-0 flex-1 overflow-y-auto">
      <div v-if="activeTab === 'runs'" class="mx-auto max-w-5xl p-5 sm:p-7">
        <div class="flex items-end justify-between gap-4">
          <div>
            <p class="text-[10px] font-bold uppercase tracking-[0.2em] text-cyan-200 light:text-cyan-800">
              {{ runHistoryUnavailable ? 'Protected activity' : 'Run monitor' }}
            </p>
            <h2 class="mt-1 text-xl font-semibold text-white light:text-gray-900">
              {{ runHistoryUnavailable ? 'Run history access required' : 'Event-by-event execution' }}
            </h2>
            <p class="mt-1 text-sm text-white/35 light:text-gray-600">
              {{ runHistoryUnavailable
                ? 'Execution history can reveal customer and follow-up activity.'
                : 'Every run is pinned to the immutable version that handled it.' }}
            </p>
          </div>
          <Button
            v-if="!runHistoryUnavailable"
            variant="outline"
            size="sm"
            class="gap-2"
            :disabled="executionsLoading || !policy"
            @click="loadExecutions"
          >
            <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': executionsLoading }" />
            Refresh
          </Button>
        </div>

        <div
          v-if="runHistoryUnavailable"
          class="mt-8 flex min-h-72 flex-col items-center justify-center rounded-2xl border border-dashed border-cyan-300/15 bg-cyan-300/[0.025] px-6 text-center"
        >
          <span class="flex h-12 w-12 items-center justify-center rounded-2xl bg-cyan-300/[0.07] text-cyan-200 light:bg-cyan-100 light:text-cyan-800">
            <LockKeyhole class="h-5 w-5" />
          </span>
          <p class="mt-4 text-sm font-medium text-white light:text-gray-900">Run history stays private</p>
          <p class="mt-1 max-w-md text-xs leading-5 text-white/35 light:text-gray-600">
            Contact read and Follow-up read permissions are both required. You can still inspect this policy and its immutable version history.
          </p>
        </div>
        <div v-else-if="executionsLoading" class="flex min-h-72 items-center justify-center">
          <Loader2 class="h-6 w-6 animate-spin text-cyan-200" />
        </div>
        <div v-else-if="!policy" class="mt-8 rounded-2xl border border-dashed border-white/10 p-10 text-center light:border-gray-300">
          <Activity class="mx-auto h-7 w-7 text-white/25 light:text-gray-400" />
          <p class="mt-3 text-sm text-white/45 light:text-gray-600">Save this policy to begin monitoring runs.</p>
        </div>
        <div v-else-if="!executions.length" class="mt-8 rounded-2xl border border-dashed border-white/10 p-10 text-center light:border-gray-300">
          <Activity class="mx-auto h-7 w-7 text-white/25 light:text-gray-400" />
          <p class="mt-3 text-sm font-medium text-white light:text-gray-900">No production runs yet</p>
          <p class="mt-1 text-xs text-white/35 light:text-gray-600">Preview is write-free and will never appear here.</p>
        </div>
        <div v-else class="mt-6 space-y-3">
          <details
            v-for="execution in executions"
            :key="execution.id"
            class="group rounded-2xl border border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white"
          >
            <summary class="flex cursor-pointer list-none items-center gap-3 p-4 outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-cyan-300">
              <span class="flex h-9 w-9 items-center justify-center rounded-xl" :class="executionStatusClass(execution.status)">
                <Check v-if="execution.status === 'completed'" class="h-4 w-4" />
                <XCircle v-else-if="execution.status === 'failed'" class="h-4 w-4" />
                <Clock3 v-else class="h-4 w-4" />
              </span>
              <span class="min-w-0 flex-1">
                <span class="flex flex-wrap items-center gap-2">
                  <span class="text-xs font-semibold capitalize text-white light:text-gray-900">{{ execution.status }}</span>
                  <span class="rounded-full bg-white/[0.05] px-2 py-0.5 text-[9px] text-white/35 light:bg-gray-100 light:text-gray-600">Version {{ execution.policy_version_number }}</span>
                </span>
                <span class="mt-1 block truncate text-[10px] text-white/30 light:text-gray-500">
                  Event {{ execution.activity_event_id }} · {{ formatDate(execution.triggered_at) }}
                </span>
              </span>
              <ChevronRight class="h-4 w-4 text-white/25 transition group-open:rotate-90 light:text-gray-400" />
            </summary>
            <div class="border-t border-white/[0.07] px-4 py-4 light:border-gray-100">
              <p
                v-if="execution.status === 'failed'"
                class="mb-3 flex items-center gap-2 rounded-xl bg-rose-300/[0.06] p-3 text-[11px] text-rose-200 light:text-rose-800"
              >
                <XCircle class="h-3.5 w-3.5 shrink-0" />
                This run recorded an error. Sensitive internal error details are not exposed here.
              </p>
              <ol v-if="execution.steps?.length" class="space-y-2">
                <li v-for="step in execution.steps" :key="step.id" class="flex items-start gap-3 rounded-xl bg-black/15 p-3 light:bg-gray-50">
                  <span class="mt-1 h-2 w-2 shrink-0 rounded-full" :class="step.status === 'completed' ? 'bg-emerald-300' : step.has_error || step.status === 'failed' ? 'bg-rose-300' : 'bg-slate-400'" />
                  <div class="min-w-0 flex-1">
                    <p class="text-xs font-medium capitalize text-white/70 light:text-gray-800">{{ step.node_type.replace('_', ' ') }}</p>
                    <p class="mt-1 text-[10px] capitalize text-white/30 light:text-gray-500">{{ step.status }} · {{ formatDate(step.completed_at || step.scheduled_at) }}</p>
                    <div v-if="step.branch || step.reason_code || step.has_error" class="mt-2 flex flex-wrap items-center gap-1.5">
                      <span
                        v-if="step.branch"
                        class="rounded-full bg-cyan-300/[0.07] px-2 py-0.5 text-[9px] text-cyan-200 light:text-cyan-800"
                      >
                        {{ executionBranchLabel(step.branch) }}
                      </span>
                      <span
                        v-if="step.reason_code"
                        class="rounded-full bg-white/[0.05] px-2 py-0.5 font-mono text-[9px] text-white/35 light:bg-gray-100 light:text-gray-600"
                      >
                        {{ step.reason_code }}
                      </span>
                      <span
                        v-if="step.has_error"
                        class="flex items-center gap-1 rounded-full bg-rose-300/[0.08] px-2 py-0.5 text-[9px] text-rose-200 light:text-rose-800"
                      >
                        <XCircle class="h-2.5 w-2.5" />
                        Error recorded
                      </span>
                    </div>
                  </div>
                  <RouterLink v-if="step.task_id" to="/crm/tasks" class="text-[10px] text-lime-200 light:text-lime-800">View task</RouterLink>
                </li>
              </ol>
            </div>
          </details>
        </div>
      </div>

      <div v-else-if="activeTab === 'versions'" class="mx-auto max-w-4xl p-5 sm:p-7">
        <div class="flex items-end justify-between gap-4">
          <div>
            <p class="text-[10px] font-bold uppercase tracking-[0.2em] text-violet-200 light:text-violet-800">Version ledger</p>
            <h2 class="mt-1 text-xl font-semibold text-white light:text-gray-900">Immutable policy history</h2>
            <p class="mt-1 text-sm text-white/35 light:text-gray-600">Publishing never rewrites the graph used by an earlier run.</p>
          </div>
          <Button variant="outline" size="sm" class="gap-2" :disabled="versionsLoading || !policy" @click="loadVersions">
            <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': versionsLoading }" />
            Refresh
          </Button>
        </div>

        <div v-if="versionsLoading" class="flex min-h-72 items-center justify-center">
          <Loader2 class="h-6 w-6 animate-spin text-violet-200" />
        </div>
        <div v-else-if="!versions.length" class="mt-8 rounded-2xl border border-dashed border-white/10 p-10 text-center light:border-gray-300">
          <FileClock class="mx-auto h-7 w-7 text-white/25 light:text-gray-400" />
          <p class="mt-3 text-sm font-medium text-white light:text-gray-900">No published versions</p>
          <p class="mt-1 text-xs text-white/35 light:text-gray-600">Save, preview and activate the first draft to create version 1.</p>
        </div>
        <ol v-else class="relative mt-8 ml-4 border-l border-white/10 pl-7 light:border-gray-200">
          <li v-for="version in versions" :key="version.id" class="relative pb-6 last:pb-0">
            <span class="absolute -left-[34px] top-1 flex h-3 w-3 rounded-full border-[3px] border-[#080b0c] bg-violet-300 light:border-[#f5f6f3]" />
            <article class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-4 light:border-gray-200 light:bg-white">
              <div class="flex items-start justify-between gap-3">
                <div>
                  <div class="flex items-center gap-2">
                    <p class="text-sm font-semibold text-white light:text-gray-900">Version {{ version.number }}</p>
                    <Badge v-if="version.number === activeVersionNumber" variant="success">Current live</Badge>
                  </div>
                  <p class="mt-1 text-[10px] text-white/30 light:text-gray-500">Published {{ formatDate(version.published_at || version.created_at) }}</p>
                </div>
                <span class="rounded-lg bg-black/20 px-2 py-1 font-mono text-[9px] text-white/30 light:bg-gray-100 light:text-gray-500">{{ version.checksum?.slice(0, 12) }}</span>
              </div>
              <div class="mt-4 flex flex-wrap gap-2">
                <span v-for="eventType in version.trigger_event_types" :key="eventType" class="rounded-full bg-cyan-300/[0.07] px-2.5 py-1 text-[10px] text-cyan-200 light:text-cyan-800">{{ eventType }}</span>
              </div>
              <p class="mt-3 text-[10px] text-white/30 light:text-gray-500">{{ version.graph?.nodes?.length ?? 0 }} steps · {{ version.graph?.edges?.length ?? 0 }} connections</p>
            </article>
          </li>
        </ol>
      </div>

      <div v-else class="mx-auto max-w-5xl p-5 sm:p-7">
        <div>
          <p class="text-[10px] font-bold uppercase tracking-[0.2em] text-emerald-200 light:text-emerald-800">Policy guardrails</p>
          <h2 class="mt-1 text-xl font-semibold text-white light:text-gray-900">Safety is part of the graph</h2>
          <p class="mt-1 max-w-2xl text-sm leading-6 text-white/35 light:text-gray-600">
            This first release deliberately automates internal work, not customer communication.
          </p>
        </div>

        <div class="mt-7 grid gap-3 md:grid-cols-2">
          <article
            v-for="guard in [
              { title: 'Task-only actions', detail: 'The runtime can only create CRM follow-up tasks.', icon: CheckSquare2, state: 'Enforced', tone: 'emerald' },
              { title: 'At-most-once execution', detail: 'Durable event receipts and per-step idempotency prevent duplicate work.', icon: ShieldCheck, state: 'Enforced', tone: 'emerald' },
              { title: 'Preview before activation', detail: 'The studio requires a current server preview before publish.', icon: Eye, state: 'Required', tone: 'cyan' },
              { title: 'Immutable live versions', detail: 'Every run stays pinned to the exact published graph it used.', icon: FileClock, state: 'Enforced', tone: 'violet' },
            ]"
            :key="guard.title"
            class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-gray-200 light:bg-white"
          >
            <div class="flex items-start justify-between gap-3">
              <span class="flex h-10 w-10 items-center justify-center rounded-xl bg-white/[0.05] light:bg-gray-100">
                <component :is="guard.icon" class="h-5 w-5" :class="`text-${guard.tone}-300`" />
              </span>
              <span class="rounded-full bg-emerald-300/10 px-2.5 py-1 text-[9px] font-bold uppercase tracking-wider text-emerald-200 light:text-emerald-800">{{ guard.state }}</span>
            </div>
            <h3 class="mt-4 text-sm font-semibold text-white light:text-gray-900">{{ guard.title }}</h3>
            <p class="mt-1.5 text-xs leading-5 text-white/35 light:text-gray-600">{{ guard.detail }}</p>
          </article>
        </div>

        <section class="mt-6 overflow-hidden rounded-2xl border border-amber-300/12 bg-amber-300/[0.035]">
          <div class="flex items-start gap-3 p-5">
            <AlertTriangle class="mt-0.5 h-5 w-5 shrink-0 text-amber-300 light:text-amber-700" />
            <div>
              <h3 class="text-sm font-semibold text-white light:text-gray-900">Check ownership before activating starter policies</h3>
              <p class="mt-1.5 max-w-3xl text-xs leading-5 text-white/40 light:text-gray-600">
                ReReply may already have a native care-continuity rule for no-shows, package risk or overdue invoices. Treat overlap warnings from the server preview as blocking until an operator confirms which rule owns the follow-up.
              </p>
            </div>
          </div>
        </section>

        <section class="mt-6 rounded-2xl border border-dashed border-white/10 p-5 opacity-65 light:border-gray-300">
          <div class="flex items-start gap-3">
            <LockKeyhole class="mt-0.5 h-5 w-5 shrink-0 text-white/40 light:text-gray-500" />
            <div>
              <h3 class="text-sm font-semibold text-white light:text-gray-900">Outbound messaging controls — future release</h3>
              <p class="mt-1.5 text-xs leading-5 text-white/35 light:text-gray-600">
                Quiet hours, channel eligibility, consent, frequency caps and approval gates will remain mandatory before any message action is unlocked.
              </p>
            </div>
          </div>
        </section>
      </div>
    </main>

    <Sheet v-model:open="showMobileInspector">
      <SheetContent side="right" class="w-[min(92vw,380px)] border-white/10 bg-[#0d1012] p-0 light:border-gray-200 light:bg-white">
        <SheetHeader class="sr-only">
          <SheetTitle>Step settings</SheetTitle>
          <SheetDescription>Configure the selected automation step.</SheetDescription>
        </SheetHeader>
        <AutomationNodeInspector
          class="pt-10"
          :catalog="catalog"
          :node="selectedNode"
          :readonly="readOnly"
          @update="updateSelectedConfig"
          @delete="deleteSelectedNode"
        />
      </SheetContent>
    </Sheet>

    <AutomationPreviewDialog
      v-model:open="showPreview"
      :loading="previewing"
      :event-types="triggerEventTypes"
      :suggested-metadata="suggestedPreviewMetadata"
      :show-contact-opt-out="usesMarketingOptOutCondition"
      :result="previewResult"
      @run="runPreview"
      @select-node="handlePreviewNode"
    />

    <ConfirmDialog
      v-model:open="showActivateConfirm"
      :title="hasPublishedVersion ? 'Publish a new live version?' : 'Activate this policy?'"
      confirm-label="Publish and activate"
      :is-submitting="mutating"
      @confirm="activate"
    >
      <template #description>
        <p>
          Draft revision {{ policy?.version }} will become an immutable live version. New matching customer events can create internal follow-up tasks.
        </p>
        <p class="mt-2 font-medium text-amber-600 dark:text-amber-300">
          Confirm that preview warnings and any native care-rule overlap have been reviewed.
        </p>
        <div
          v-if="previewWarningMessages.length"
          class="mt-3 rounded-lg border border-amber-300/20 bg-amber-300/[0.06] p-3 text-left"
        >
          <p class="text-xs font-semibold text-amber-700 dark:text-amber-200">
            Warnings from the required preview
          </p>
          <ul class="mt-1.5 list-disc space-y-1 pl-4 text-xs text-amber-700/80 dark:text-amber-100/70">
            <li v-for="warning in previewWarningMessages" :key="warning">{{ warning }}</li>
          </ul>
        </div>
      </template>
    </ConfirmDialog>

    <ConfirmDialog
      v-model:open="showDeleteConfirm"
      title="Delete this draft policy?"
      description="This unpublished draft will be permanently removed. Published policies cannot be deleted here."
      confirm-label="Delete draft"
      variant="destructive"
      :is-submitting="mutating"
      @confirm="deleteDraft"
    />

    <UnsavedChangesDialog :open="showLeaveDialog" @stay="cancelLeave" @leave="confirmLeave" />
  </div>
</template>

<style scoped>
.automation-studio :deep(.vue-flow__edge-path) {
  transition: stroke 180ms ease, stroke-width 180ms ease;
}

.automation-studio :deep(.vue-flow__edge.selected .vue-flow__edge-path) {
  stroke: #67e8f9 !important;
  stroke-width: 3 !important;
}

.automation-studio :deep(.vue-flow__controls) {
  overflow: hidden;
  border: 1px solid rgba(255, 255, 255, 0.1);
  border-radius: 0.75rem;
  background: rgba(10, 14, 16, 0.9);
  box-shadow: 0 15px 35px rgba(0, 0, 0, 0.25);
}

.automation-studio :deep(.vue-flow__controls-button) {
  border-color: rgba(255, 255, 255, 0.07);
  background: transparent;
  fill: rgba(255, 255, 255, 0.6);
}

.automation-studio :deep(.vue-flow__minimap) {
  overflow: hidden;
  border: 1px solid rgba(255, 255, 255, 0.09);
  border-radius: 0.75rem;
  background: rgba(10, 14, 16, 0.88);
}

.automation-studio :deep(.vue-flow__minimap-mask) {
  fill: rgba(15, 20, 18, 0.72);
}

.automation-studio :deep(.vue-flow__minimap-node) {
  fill: #a3e635;
  stroke: rgba(217, 249, 157, 0.65);
}

:global(.light) .automation-studio :deep(.vue-flow__controls),
:global(.light) .automation-studio :deep(.vue-flow__minimap) {
  border-color: rgb(229 231 235);
  background: rgba(255, 255, 255, 0.92);
}

:global(.light) .automation-studio :deep(.vue-flow__minimap-mask) {
  fill: rgba(226, 232, 240, 0.7);
}

@media (max-width: 640px) {
  .automation-studio :deep(.vue-flow__minimap) {
    display: none;
  }
}

@media (prefers-reduced-motion: reduce) {
  .automation-studio *,
  .automation-studio *::before,
  .automation-studio *::after {
    scroll-behavior: auto !important;
    transition-duration: 0.01ms !important;
    animation-duration: 0.01ms !important;
  }
}
</style>
