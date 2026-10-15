/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import MediaSendDialog from './MediaSendDialog.vue'
import { validateChatAttachment } from '@/lib/chatAttachments'
import type { StagedAttachment, StagedAttachmentStatus } from '@/composables/useChatAttachments'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, named?: Record<string, unknown>) => (named ? `${key} ${JSON.stringify(named)}` : key),
  }),
}))

const MiB = 1024 * 1024

let sequence = 0
function item(
  name: string,
  type: string,
  overrides: Partial<StagedAttachment> & { size?: number } = {},
): StagedAttachment {
  const { size = 2048, ...rest } = overrides
  const file = new File(['x'], name, { type })
  Object.defineProperty(file, 'size', { value: size })
  const validation = validateChatAttachment(file)
  const status: StagedAttachmentStatus = validation.ok ? 'ready' : 'invalid'
  return {
    id: `item-${++sequence}`,
    file,
    previewUrl: null,
    caption: '',
    asDocument: false,
    validation,
    status,
    progress: 0,
    errorMessage: null,
    ...rest,
  }
}

const DialogStub = { props: ['open'], emits: ['update:open'], template: '<div v-if="open" data-stub="dialog"><slot /></div>' }
const Passthrough = { template: '<div><slot /></div>' }

function mountDialog(props: Partial<InstanceType<typeof MediaSendDialog>['$props']> & { items: StagedAttachment[] }) {
  return mount(MediaSendDialog, {
    props: { open: true, recipientName: 'Aisyah binti Ahmad', recipientDetail: '+60 12-345 6789', ...props },
    global: {
      stubs: {
        Dialog: DialogStub,
        DialogContent: Passthrough,
        DialogHeader: Passthrough,
        DialogTitle: Passthrough,
        DialogDescription: Passthrough,
      },
    },
  })
}

function buttonByTestId(wrapper: ReturnType<typeof mountDialog>, testId: string) {
  return wrapper.get(`[data-testid="${testId}"]`)
}

describe('MediaSendDialog', () => {
  it('names the recipient so the agent can confirm who gets the files', () => {
    const wrapper = mountDialog({ items: [item('xray.jpg', 'image/jpeg')] })
    const recipient = wrapper.get('[data-testid="composer-media-recipient"]')
    expect(recipient.text()).toContain('Aisyah binti Ahmad')
    expect(recipient.text()).toContain('+60 12-345 6789')
    expect(wrapper.text()).toContain('chat.sendMedia')
  })

  it('lists each file with a remove button and a caption field where WhatsApp allows one', async () => {
    const photo = item('xray.jpg', 'image/jpeg')
    const voice = item('note.ogg', 'audio/ogg')
    const pdf = item('lab.pdf', 'application/pdf')
    const wrapper = mountDialog({ items: [photo, voice, pdf] })

    const rows = wrapper.findAll('[data-testid="composer-media-item"]')
    expect(rows).toHaveLength(3)
    expect(rows[0].text()).toContain('xray.jpg')
    expect(rows[0].find('[data-testid="composer-media-caption"]').exists()).toBe(true)
    expect(rows[1].find('[data-testid="composer-media-caption"]').exists()).toBe(false)
    expect(rows[2].find('[data-testid="composer-media-caption"]').exists()).toBe(true)

    await rows[0].get('[data-testid="composer-media-caption"]').setValue('Your X-ray')
    expect(wrapper.emitted('update:caption')).toEqual([[photo.id, 'Your X-ray']])

    await rows[1].get('[data-testid="composer-media-remove"]').trigger('click')
    expect(wrapper.emitted('remove')).toEqual([[voice.id]])
    expect(rows[1].get('[data-testid="composer-media-remove"]').attributes('aria-label')).toContain('note.ogg')
  })

  it('explains rejected files and offers "send as document" only where WhatsApp accepts it', async () => {
    const heic = item('IMG_0001.HEIC', 'image/heic')
    const big = item('scan.jpg', 'image/jpeg', { size: 8 * MiB })
    const wrapper = mountDialog({ items: [heic, big, item('ok.png', 'image/png')] })
    const rows = wrapper.findAll('[data-testid="composer-media-item"]')

    expect(rows[0].text()).toContain('chat.composer.problem.unsupportedImage')
    expect(rows[0].find('[data-testid="composer-media-as-document"]').exists()).toBe(false)
    expect(rows[1].text()).toContain('chat.composer.problem.tooLarge')
    await rows[1].get('[data-testid="composer-media-as-document"]').trigger('click')
    expect(wrapper.emitted('send-as-document')).toEqual([[big.id]])
    expect(wrapper.text()).toContain('chat.composer.skippedMany {"count":2}')
  })

  it('sends the ready files', async () => {
    const wrapper = mountDialog({ items: [item('a.png', 'image/png'), item('b.png', 'image/png')] })
    const send = buttonByTestId(wrapper, 'composer-media-send')
    expect(send.text()).toContain('chat.composer.sendMany {"count":2}')
    await send.trigger('click')
    expect(wrapper.emitted('send')).toHaveLength(1)
  })

  it('shows per-file progress and blocks closing while uploading', async () => {
    const wrapper = mountDialog({
      sending: true,
      items: [
        item('a.png', 'image/png', { status: 'sent', progress: 100 }),
        item('b.png', 'image/png', { status: 'sending', progress: 40 }),
        item('c.png', 'image/png'),
      ],
    })
    const rows = wrapper.findAll('[data-testid="composer-media-item"]')
    expect(rows[0].text()).toContain('chat.composer.sent')
    expect(rows[1].text()).toContain('chat.composer.uploading {"progress":40}')
    expect(rows[1].find('[role="progressbar"]').exists()).toBe(true)
    expect(buttonByTestId(wrapper, 'composer-media-send').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="composer-media-add"]').exists()).toBe(false)
    expect(rows[2].get('[data-testid="composer-media-remove"]').attributes('disabled')).toBeDefined()

    const cancel = wrapper.findAll('button').find(button => button.text() === 'common.close')!
    expect(cancel.attributes('disabled')).toBeDefined()
    await cancel.trigger('click')
    expect(wrapper.emitted('update:open')).toBeUndefined()
  })

  it('reports failures per file and retries only them', async () => {
    const wrapper = mountDialog({
      items: [
        item('a.png', 'image/png', { status: 'sent', progress: 100 }),
        item('b.png', 'image/png', { status: 'failed', errorMessage: 'Media upload failed' }),
      ],
    })
    const rows = wrapper.findAll('[data-testid="composer-media-item"]')
    expect(rows[1].get('[role="alert"]').text()).toContain('Media upload failed')
    // Nothing is ready, so only the retry action is offered.
    expect(wrapper.find('[data-testid="composer-media-send"]').exists()).toBe(false)
    const retry = buttonByTestId(wrapper, 'composer-media-retry')
    expect(retry.text()).toContain('chat.composer.retryFailed {"count":1}')
    await retry.trigger('click')
    expect(wrapper.emitted('retry-failed')).toHaveLength(1)

    const close = wrapper.findAll('button').find(button => button.text() === 'common.close')!
    await close.trigger('click')
    expect(wrapper.emitted('update:open')).toEqual([[false]])
  })

  it('offers "Add files" until the 10-file limit', async () => {
    const wrapper = mountDialog({ items: [item('a.png', 'image/png')] })
    await buttonByTestId(wrapper, 'composer-media-add').trigger('click')
    expect(wrapper.emitted('add-more')).toHaveLength(1)

    const full = mountDialog({ items: Array.from({ length: 10 }, (_, i) => item(`p${i}.png`, 'image/png')) })
    expect(full.find('[data-testid="composer-media-add"]').exists()).toBe(false)
  })

  it('shows thumbnails for images and videos, icons otherwise', () => {
    const wrapper = mountDialog({
      items: [
        item('a.png', 'image/png', { previewUrl: 'blob:a' }),
        item('v.mp4', 'video/mp4', { previewUrl: 'blob:v' }),
        item('lab.pdf', 'application/pdf'),
      ],
    })
    const rows = wrapper.findAll('[data-testid="composer-media-item"]')
    expect(rows[0].get('img').attributes('src')).toBe('blob:a')
    expect(rows[1].get('video').attributes('src')).toBe('blob:v')
    expect(rows[2].find('[class*="lucide-file-text"]').exists()).toBe(true)
  })

  it('does not render the composer-restricted icons on its per-file buttons', () => {
    const wrapper = mountDialog({ items: [item('a.png', 'image/png')] })
    const row = wrapper.get('[data-testid="composer-media-item"]')
    for (const icon of ['lucide-x', 'lucide-send', 'lucide-paperclip', 'lucide-smile', 'lucide-reply']) {
      expect(row.find(`[class*="${icon}"]`).exists(), icon).toBe(false)
    }
  })
})
