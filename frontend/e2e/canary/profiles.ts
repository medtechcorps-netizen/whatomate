// @ts-nocheck -- also executed by Node's built-in TypeScript stripping.
import { createHash } from 'node:crypto'
import { readFileSync, statSync } from 'node:fs'
import { dirname, isAbsolute, relative, resolve } from 'node:path'
import { assertPrivateLocation, assertPrivatePermissions, refuseAmbient } from './private-files.mjs'

export function canonicalOrigin(raw) {
  let url
  try { url = new URL(raw) } catch { throw new Error('Canary origin must be an HTTP(S) origin') }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash || url.pathname !== '/' || url.origin !== raw) {
    throw new Error('Canary origin must be canonical, with no path or credentials')
  }
  if (url.hostname === 'rereply.app' || url.hostname.endsWith('.rereply.app')) throw new Error('Production origins are forbidden')
  return url
}

export function validateProfile({ profile, origin, namespace, originHash, stubOrigin }) {
  if (!['local', 'staging'].includes(profile)) throw new Error('CANARY_PROFILE must be local or staging')
  const url = canonicalOrigin(origin)
  if (!new RegExp(`^rereply-${profile}-[a-z0-9][a-z0-9-]{0,25}$`).test(namespace) || namespace === 'rereply-canary') {
    throw new Error('Canary namespace must be synthetic and profile-specific')
  }
  if (profile === 'local' && !['localhost', '127.0.0.1', '[::1]'].includes(url.hostname)) throw new Error('Local canaries require a loopback origin')
  if (profile === 'staging' && (url.protocol !== 'https:' || createHash('sha256').update(origin).digest('hex') !== originHash)) {
    throw new Error('Staging origin does not match the private setup file')
  }
  let stub
  try { stub = new URL(stubOrigin) } catch { throw new Error('Invalid stub endpoint') }
  canonicalOrigin(stub.origin)
  if (stub.username || stub.password || stub.search || stub.hash || !['/', '/_stub', '/_stub/'].includes(stub.pathname)) throw new Error('Invalid stub endpoint')
  if (profile === 'local' && !['localhost', '127.0.0.1', '[::1]'].includes(stub.hostname)) throw new Error('Local stub must be on loopback')
  if (profile === 'staging' && (stub.origin !== origin || stub.pathname.replace(/\/$/, '') !== '/_stub')) throw new Error('Staging stub must use the same origin at /_stub')
  return { profile, origin, namespace, stubOrigin: stubOrigin.replace(/\/$/, '') }
}

export function readPrivateFile(path) {
  assertPrivateLocation(path, { mustExist: true })
  assertPrivatePermissions(dirname(path))
  assertPrivatePermissions(path)
  const info = statSync(path)
  if (!info.isFile() || info.size > 256 * 1024) throw new Error('Invalid private canary file')
  if (process.platform !== 'win32' && (info.mode & 0o077)) throw new Error('Private canary file must have mode 0600')
  try { return JSON.parse(readFileSync(path, 'utf8')) }
  catch { throw new Error('Invalid private canary JSON') }
}

export function loadProfile(env = process.env) {
  refuseAmbient(env)
  const privateFile = env.CANARY_PRIVATE_FILE
  const saved = readPrivateFile(privateFile)
  if (saved.schema_version !== 1) throw new Error('Private canary schema differs')
  const values = saved.canary || {}
  const profile = env.CANARY_PROFILE
  if (profile === 'local') {
    if (!env.RUNNER_TEMP || !isAbsolute(env.RUNNER_TEMP)) throw new Error('Local provisioning requires an absolute RUNNER_TEMP')
    assertPrivateLocation(env.RUNNER_TEMP, { mustExist: true })
    const suffix = relative(resolve(env.RUNNER_TEMP), resolve(privateFile))
    if (!suffix || suffix.startsWith('..') || isAbsolute(suffix)) throw new Error('Local private file must be inside RUNNER_TEMP')
  }
  const selected = validateProfile({
    profile,
    origin: env.CANARY_ORIGIN || values.origin,
    namespace: env.CANARY_NAMESPACE || values.namespace,
    originHash: saved.origin_sha256,
    stubOrigin: env.CANARY_STUB_ORIGIN || values.stub_origin,
  })
  return { ...selected, privateFile, saved, values,
    stubControlKey: env.CANARY_STUB_CONTROL_KEY || values.stub_control_key,
    stubAccessToken: env.CANARY_STUB_ACCESS_TOKEN || values.stub_access_token,
    stubAppSecret: env.CANARY_STUB_APP_SECRET || values.stub_app_secret,
    configPath: env.CANARY_CONFIG_PATH || values.config_path,
    klinikOrganizationID: env.CANARY_KLINIK_ORGANIZATION_ID || values.klinik_organization_id,
  }
}
