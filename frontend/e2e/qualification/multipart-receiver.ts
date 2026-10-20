import { createHash } from 'node:crypto'
import { createServer, request as httpRequest, type ServerResponse } from 'node:http'
import type { Socket } from 'node:net'

export const digest = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex')

export type Upload = {
  ordinal: number
  organization: string
  contact: string
  kind: string
  filename: string
  fileBytes: number
  fileSha256: string
  bodyBytes: number
  bodyComplete: boolean
  responseClosed: boolean
  closedBeforeResponseEnd: boolean
  responseStatus: number | null
}

// A test-owned receiver, not the media handler or a provider simulator. The
// browser sends real multipart bytes over loopback; only acceptance/responses
// are synthetic. Static app bytes and security headers pass through unchanged.
export async function multipartReceiver(upstreamValue: string) {
  const upstream = new URL(upstreamValue)
  if (upstream.protocol !== 'http:' || upstream.hostname !== '127.0.0.1' ||
      !upstream.port || upstream.pathname !== '/' || upstream.search || upstream.hash ||
      upstream.username || upstream.password) throw new Error('owned-loopback-app-required')
  const uploads: Upload[] = []
  const faults: string[] = []
  const replies = new Map<number, ServerResponse>()
  const sockets = new Set<Socket>()
  const proxies = new Set<ReturnType<typeof httpRequest>>()
  let proxiedBytes = 0
  let proxyRequests = 0
  let closed = false
  const server = createServer((req, res) => {
    const path = req.url ?? ''
    if (!path.startsWith('/') || path.startsWith('//') || path.includes('\\')) {
      faults.push('invalid-path'); res.writeHead(400).end(); return
    }
    const url = new URL(path, upstream)
    if (url.pathname === '/__multipart/empty' && req.method === 'GET') {
      res.writeHead(200, { 'content-type': 'text/html', 'cache-control': 'no-store' })
        .end('<!doctype html><title>Synthetic identity writer</title>')
      return
    }
    if (url.pathname === '/api/messages/media' && req.method === 'POST' && !url.search) {
      if (uploads.length >= 8) { faults.push('upload-count-cap'); res.writeHead(413).end(); return }
      const record: Upload = { ordinal: uploads.length + 1, organization: '', contact: '', kind: '',
        filename: '', fileBytes: 0, fileSha256: '', bodyBytes: 0, bodyComplete: false,
        responseClosed: false, closedBeforeResponseEnd: false, responseStatus: null }
      uploads.push(record)
      replies.set(record.ordinal, res)
      res.on('close', () => {
        record.responseClosed = true
        record.closedBeforeResponseEnd = !res.writableEnded
      })
      const chunks: Buffer[] = []
      req.on('data', (chunk: Buffer) => {
        record.bodyBytes += chunk.length
        if (record.bodyBytes > 128 * 1024) {
          faults.push('multipart-byte-cap'); req.destroy(); return
        }
        chunks.push(chunk)
      })
      req.on('error', () => { if (!closed && !req.aborted) faults.push('request-error') })
      req.on('end', () => {
        void (async () => {
          const contentType = req.headers['content-type'] ?? ''
          if (!/^multipart\/form-data;\s*boundary=/i.test(contentType)) throw new Error('multipart-type')
          const parsed = await new Request('http://127.0.0.1/synthetic', {
            method: 'POST', headers: { 'content-type': contentType },
            body: new Uint8Array(Buffer.concat(chunks)),
          }).formData()
          const keys = [...parsed.keys()].sort()
          if (JSON.stringify(keys) !== JSON.stringify(['contact_id', 'file', 'type', 'whatsapp_account'])) {
            throw new Error('multipart-fields')
          }
          const file = parsed.get('file')
          if (!(file instanceof File)) throw new Error('multipart-file')
          record.organization = String(req.headers['x-organization-id'] ?? '')
          record.contact = String(parsed.get('contact_id'))
          record.kind = String(parsed.get('type'))
          if (parsed.get('whatsapp_account') !== 'synthetic-upload') throw new Error('multipart-account')
          record.filename = file.name
          record.fileBytes = file.size
          record.fileSha256 = digest(new Uint8Array(await file.arrayBuffer()))
          record.bodyComplete = req.complete
        })().catch(() => { faults.push('multipart-parse'); if (!res.destroyed) res.writeHead(400).end() })
      })
      return
    }
    // No API request can reach the upstream app (and hence its database).
    // Only fixed SPA paths and Vite's emitted assets are allowed through.
    const staticPath = /^\/(?:chat(?:\/[a-f0-9-]+)?|assets\/[A-Za-z0-9._/-]+|brand\/rereply-mark\.png|theme-init\.js|notification\.mp3|favicon\.(?:ico|svg))?$/.test(url.pathname)
    if (!['GET', 'HEAD'].includes(req.method ?? '') || !staticPath || url.search) {
      faults.push('unexpected-upstream-path'); res.writeHead(404).end(); return
    }
    if (++proxyRequests > 512) { faults.push('proxy-count-cap'); res.writeHead(429).end(); return }
    // Do not forward browser credentials, authorization, or customer headers.
    const proxy = httpRequest(url, { method: req.method, headers: { 'accept-encoding': 'identity' },
      timeout: 10_000 }, response => {
      const chunks: Buffer[] = []
      let size = 0
      response.on('data', (chunk: Buffer) => {
        size += chunk.length; proxiedBytes += chunk.length
        if (size > 16 * 1024 * 1024 || proxiedBytes > 64 * 1024 * 1024) {
          faults.push('proxy-byte-cap'); response.destroy(); proxy.destroy(); res.destroy(); return
        }
        chunks.push(chunk)
      })
      response.on('end', () => {
        if (!res.destroyed) res.writeHead(response.statusCode ?? 502, response.headers).end(Buffer.concat(chunks))
      })
      response.on('error', () => { if (!closed) faults.push('proxy-response-error'); res.destroy() })
    })
    proxies.add(proxy)
    proxy.on('close', () => proxies.delete(proxy))
    proxy.on('timeout', () => { faults.push('proxy-timeout'); proxy.destroy() })
    proxy.on('error', () => { if (!closed) faults.push('proxy-error'); res.destroy() })
    proxy.end()
  })
  server.requestTimeout = 15_000
  server.headersTimeout = 10_000
  server.on('connection', socket => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)) })
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => { server.removeListener('error', reject); resolve() })
  })
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('receiver-address')
  return {
    origin: `http://127.0.0.1:${address.port}`, uploads, faults,
    respond(ordinal: number, status: number, body: unknown, json = true) {
      const response = replies.get(ordinal)
      const record = uploads[ordinal - 1]
      if (!response || !record?.bodyComplete || response.destroyed || response.headersSent) {
        throw new Error('receiver-response-state')
      }
      record.responseStatus = status
      response.writeHead(status, { 'content-type': json ? 'application/json' : 'text/plain', 'cache-control': 'no-store' })
        .end(json ? JSON.stringify(body) : String(body))
    },
    loseResponse(ordinal: number) {
      const response = replies.get(ordinal)
      if (!response || !uploads[ordinal - 1]?.bodyComplete) throw new Error('receiver-loss-state')
      response.destroy()
    },
    async close() {
      closed = true
      for (const proxy of proxies) proxy.destroy()
      const close = new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()))
      for (const socket of sockets) socket.destroy()
      let timer: ReturnType<typeof setTimeout> | undefined
      try {
        await Promise.race([close, new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new Error('receiver-close-deadline')), 5_000)
        })])
        // Closing an owned socket and observing its close event are distinct.
        const deadline = Date.now() + 1_000
        while ((sockets.size || proxies.size) && Date.now() < deadline) {
          await new Promise<void>(resolve => setTimeout(resolve, 10))
        }
        if (sockets.size || proxies.size || server.listening) throw new Error('receiver-not-joined')
        return { closed: true, sockets: sockets.size, proxies: proxies.size, proxyRequests, proxiedBytes }
      } finally { clearTimeout(timer) }
    },
  }
}
