/** @vitest-environment happy-dom */
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({ ice: vi.fn(), initiate: vi.fn(), connect: vi.fn(), hangup: vi.fn() }))
vi.mock('@/services/api', () => ({
  callLogsService: {}, ivrFlowsService: {},
  callTransfersService: { connect: mocks.connect, hangup: mocks.hangup },
  outgoingCallsService: { getICEServers: mocks.ice, initiate: mocks.initiate, hangup: mocks.hangup },
}))

const peerConfigurations: RTCConfiguration[] = []
class Peer {
  iceGatheringState = 'complete'
  localDescription = { sdp: 'synthetic-sdp' }
  constructor(config: RTCConfiguration) { peerConfigurations.push(config) }
  addTrack() {}
  async createOffer() { return { type: 'offer', sdp: 'synthetic-sdp' } }
  async setLocalDescription() {}
  async setRemoteDescription() {}
  close() {}
}

function iceResponse(credential = 'temporary-password') {
  return { data: { data: {
    ice_servers: [{ urls: ['turns:turn.cloudflare.com:443?transport=tcp'], username: 'temporary-user', credential }],
    expires_at: new Date(Date.now() + 24 * 3600_000).toISOString(),
    ice_transport_policy: 'relay',
  } } }
}

describe('new-call ICE credential renewal', () => {
  let stop: ReturnType<typeof vi.fn>
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.resetAllMocks()
    peerConfigurations.length = 0
    stop = vi.fn()
    const track = { stop }
    vi.stubGlobal('navigator', { language: 'en-MY', mediaDevices: { getUserMedia: vi.fn().mockImplementation(async () => ({ getTracks: () => [track], getAudioTracks: () => [track] })) } })
    vi.stubGlobal('RTCPeerConnection', Peer)
    vi.stubGlobal('RTCSessionDescription', class { constructor(value: unknown) { Object.assign(this, value) } })
    mocks.ice.mockResolvedValue(iceResponse())
    mocks.initiate.mockResolvedValue({ data: { data: { call_log_id: 'call', sdp_answer: 'synthetic-answer' } } })
    mocks.connect.mockResolvedValue({ data: { data: { sdp_answer: 'synthetic-answer' } } })
  })
  afterEach(() => { vi.unstubAllGlobals() })

  it('fetches again for the next call and applies the server relay policy', async () => {
    const { useCallingStore } = await import('./calling')
    const store = useCallingStore()
    await store.makeOutgoingCall('contact', 'name', 'account')
    await store.endCall()
    mocks.ice.mockResolvedValueOnce(iceResponse('renewed-password'))
    await store.makeOutgoingCall('contact', 'name', 'account')
    expect(mocks.ice).toHaveBeenCalledTimes(2)
    expect(peerConfigurations[1]).toMatchObject({ iceTransportPolicy: 'relay', iceServers: [{ credential: 'renewed-password' }] })
    await store.endCall()
  })

  it('does not cache a relay failure and retains a waiting transfer for retry', async () => {
    const { useCallingStore } = await import('./calling')
    const store = useCallingStore()
    store.handleCallEvent('call_transfer_waiting', { id: 'transfer', status: 'waiting' })
    mocks.ice.mockRejectedValueOnce(new Error('relay unavailable'))
    await expect(store.acceptTransfer('transfer')).rejects.toThrow('relay unavailable')
    expect(stop).toHaveBeenCalled()
    expect(peerConfigurations).toHaveLength(0)
    expect(store.waitingTransfers).toHaveLength(1)
    await store.acceptTransfer('transfer')
    expect(mocks.ice).toHaveBeenCalledTimes(2)
    expect(mocks.connect).toHaveBeenCalledOnce()
    await store.endCall()
  })

  it('rejects expired credentials before creating a peer and releases the microphone', async () => {
    const { useCallingStore } = await import('./calling')
    const store = useCallingStore()
    const expired = iceResponse()
    expired.data.data.expires_at = new Date(Date.now() - 1000).toISOString()
    mocks.ice.mockResolvedValueOnce(expired)
    await expect(store.makeOutgoingCall('contact', 'name', 'account')).rejects.toThrow('expired')
    expect(peerConfigurations).toHaveLength(0)
    expect(stop).toHaveBeenCalled()
    expect(mocks.initiate).not.toHaveBeenCalled()
  })

  it('rejects duplicate accepts while relay preparation is pending', async () => {
    let resolve!: (value: ReturnType<typeof iceResponse>) => void
    mocks.ice.mockReturnValueOnce(new Promise(res => { resolve = res }))
    const { useCallingStore } = await import('./calling')
    const store = useCallingStore()
    store.handleCallEvent('call_transfer_waiting', { id: 'transfer', status: 'waiting' })
    const first = store.acceptTransfer('transfer')
    await Promise.resolve()
    await Promise.resolve()
    await expect(store.acceptTransfer('transfer')).rejects.toThrow('already in progress')
    expect(navigator.mediaDevices.getUserMedia).toHaveBeenCalledOnce()
    expect(mocks.ice).toHaveBeenCalledOnce()
    resolve(iceResponse())
    await first
    expect(peerConfigurations).toHaveLength(1)
    expect(mocks.connect).toHaveBeenCalledOnce()
    await store.endCall()
  })

  it('an old pending failure cannot stop or unlock a new identity call setup', async () => {
    let rejectOld!: (reason: Error) => void
    let resolveNew!: (value: ReturnType<typeof iceResponse>) => void
    mocks.ice.mockReturnValueOnce(new Promise((_resolve, reject) => { rejectOld = reject }))
      .mockReturnValueOnce(new Promise(resolve => { resolveNew = resolve }))
    const oldStop = vi.fn(), newStop = vi.fn()
    const stream = (stopTrack: ReturnType<typeof vi.fn>) => ({ getTracks: () => [{ stop: stopTrack }], getAudioTracks: () => [{ stop: stopTrack }] })
    vi.mocked(navigator.mediaDevices.getUserMedia).mockResolvedValueOnce(stream(oldStop) as unknown as MediaStream)
      .mockResolvedValueOnce(stream(newStop) as unknown as MediaStream)
    const { useCallingStore } = await import('./calling')
    const store = useCallingStore()
    const old = store.acceptTransfer('old')
    await Promise.resolve(); await Promise.resolve()
    store.resetForIdentityChange()
    const current = store.acceptTransfer('new')
    await Promise.resolve(); await Promise.resolve()
    rejectOld(new Error('old relay request failed'))
    await expect(old).rejects.toThrow('old relay request failed')
    await expect(store.acceptTransfer('duplicate-new')).rejects.toThrow('already in progress')
    expect(newStop).not.toHaveBeenCalled()
    resolveNew(iceResponse())
    await current
    expect(peerConfigurations).toHaveLength(1)
    await store.endCall()
  })

  it('ignores an old-identity relay response and does not restore its transfer', async () => {
    let resolve!: (value: ReturnType<typeof iceResponse>) => void
    mocks.ice.mockReturnValueOnce(new Promise(res => { resolve = res }))
    const { useCallingStore } = await import('./calling')
    const store = useCallingStore()
    store.handleCallEvent('call_transfer_waiting', { id: 'transfer', status: 'waiting' })
    const pending = store.acceptTransfer('transfer')
    await Promise.resolve()
    await Promise.resolve()
    store.resetForIdentityChange()
    resolve(iceResponse())
    await expect(pending).rejects.toThrow('Calling identity changed')
    expect(peerConfigurations).toHaveLength(0)
    expect(store.waitingTransfers).toHaveLength(0)
    expect(stop).toHaveBeenCalled()
  })
})
