import { createApp, defineComponent, h, ref } from 'vue'
import { createI18n } from 'vue-i18n'
import MessageContextMenu from '../../src/components/chat/MessageContextMenu.vue'
import { ScrollArea } from '../../src/components/ui/scroll-area'
import type { Message } from '../../src/stores/contacts'
import '../../src/assets/index.css'

const messages: Message[] = Array.from({ length: 15 }, (_, index) => ({
  id: `synthetic-${index}`, contact_id: 'synthetic-contact', direction: 'incoming',
  message_type: 'text', content: { body: `Synthetic message ${index} with selectable text` },
  status: 'delivered', created_at: '2026-10-27T00:00:00Z', updated_at: '2026-10-27T00:00:00Z',
}))

createApp(defineComponent({
  setup() {
    const transcript = ref<HTMLElement | null>(null)
    const composer = ref<HTMLElement | null>(null)
    const scope = ref('synthetic-org/contact-a')
    const actions = ref<string[]>([])
    return () => h('main', { style: 'width:340px;margin:20px' }, [
      h(ScrollArea, { class: 'h-64 w-full border', orientation: 'vertical' }, () =>
        h('div', { ref: transcript, 'data-testid': 'chat-message-list' }, messages.map(message =>
          h('div', { 'data-testid': 'chat-message', 'data-message-id': message.id, style: 'padding:12px' }, [
            h('div', { class: 'chat-bubble', style: 'padding:12px;background:#eee;user-select:text' }, [
              h('span', { 'data-testid': message.id }, message.content.body),
              h('a', { href: '#native', 'data-testid': `link-${message.id}` }, ' native link'),
            ]),
          ]),
        )),
      ),
      h('textarea', { ref: composer, 'data-testid': 'composer', 'aria-label': 'Message' }),
      h('button', { 'data-testid': 'switch-scope', onClick: () => { scope.value += '/next' } }, 'Switch scope'),
      h('output', { 'data-testid': 'actions' }, JSON.stringify(actions.value)),
      h(MessageContextMenu, {
        container: transcript.value, composer: composer.value, messages, scopeKey: scope.value,
        quickReactions: ['👍', '🙏'], canForward: true,
        onReply: (message: Message) => actions.value.push(`reply:${message.id}`),
        onReact: (id: string, emoji: string) => actions.value.push(`react:${id}:${emoji}`),
        onForward: (message: Message) => actions.value.push(`forward:${message.id}`),
      }),
    ])
  },
})).use(createI18n({
  legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false,
  messages: { en: { chat: { reply: 'Reply', forward: { action: 'Forward' }, messageActions: {
    copyText: 'Copy text', react: 'React', reactWith: 'React with {emoji}', copied: 'Copied', copyFailed: 'Copy failed',
  } } } },
})).mount('#app')
