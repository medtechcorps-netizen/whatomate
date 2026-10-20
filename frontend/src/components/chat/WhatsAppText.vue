<script lang="ts">
import { computed, defineComponent, h, type VNodeChild } from 'vue'
import { parseWhatsAppFormat, type WhatsAppNode } from '@/lib/whatsappFormat'

// Renders WhatsApp-formatted text as VNodes. Text always goes through Vue's
// text escaping; there is no v-html anywhere in this path.

const CODE_CLASS = 'rounded bg-black/15 px-1 font-mono text-[0.92em]'
const MONO_CLASS = 'font-mono text-[0.92em]'

function renderNodes(nodes: WhatsAppNode[]): VNodeChild[] {
  return nodes.map((node): VNodeChild => {
    switch (node.type) {
      case 'text':
        return node.text
      case 'bold':
        return h('strong', { class: 'font-semibold' }, renderNodes(node.children))
      case 'italic':
        return h('em', renderNodes(node.children))
      case 'strike':
        return h('s', renderNodes(node.children))
      case 'code':
        return h('code', { class: CODE_CLASS }, node.text)
      case 'mono':
        return h('code', { class: MONO_CLASS, 'data-wa-mono': '' }, node.text)
    }
  })
}

export default defineComponent({
  name: 'WhatsAppText',
  props: {
    text: { type: String, default: '' },
  },
  setup(props) {
    const nodes = computed(() => parseWhatsAppFormat(props.text))
    return () => h('span', { 'data-wa-text': '' }, renderNodes(nodes.value))
  },
})
</script>
