// Disposable local/CI setup only. Nothing accepts a remote database or reads
// production configuration. All generated credentials stay in RUNNER_TEMP.
import { randomBytes } from 'node:crypto'
import { copyFileSync, mkdirSync, mkdtempSync, openSync, readFileSync } from 'node:fs'
import { isAbsolute, join, resolve } from 'node:path'
import { execFileSync, spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import pg from 'pg'
import { assertPrivateLocation, assertPrivatePermissions, atomicWritePrivate, childEnvironment, refuseAmbient, secureWindowsPath } from './private-files.mjs'

refuseAmbient()
const root = fileURLToPath(new URL('../../../', import.meta.url))
const temp = process.env.RUNNER_TEMP
if (!temp || !isAbsolute(temp)) throw new Error('An absolute RUNNER_TEMP is required')
assertPrivateLocation(temp)
const directory = join(temp, 'canary')
const privateFile = join(directory, 'private.json')
const secret = () => randomBytes(24).toString('hex')
const write = (name, value) => atomicWritePrivate(join(directory, name), value)
const quote = value => JSON.stringify(value)
const port = (name, fallback) => {
  const value = Number(process.env[name] || fallback)
  if (!Number.isInteger(value) || value < 1024 || value > 65535) throw new Error(`Invalid local ${name}`)
  return value
}

async function configure() {
  mkdirSync(directory, { recursive: true, mode: 0o700 })
  secureWindowsPath(directory, true)
  assertPrivatePermissions(directory)
  mkdirSync(join(directory, 'uploads'), { recursive: true, mode: 0o700 })
  const backendPort = port('CANARY_BACKEND_PORT', 8080), stubPort = port('CANARY_STUB_PORT', 8090)
  const databasePort = port('CANARY_PG_PORT', 5432), redisPort = port('CANARY_REDIS_PORT', 6379)
  const ownerPassword = secret(), runtimePassword = secret(), adminPassword = secret(), jwtSecret = secret(), encryptionKey = secret()
  const controlKey = secret(), accessToken = secret(), appSecret = secret()
  if (process.env.GITHUB_ACTIONS === 'true') for (const value of [ownerPassword, runtimePassword, adminPassword, jwtSecret, encryptionKey, controlKey, accessToken, appSecret]) console.log('::add-mask::' + value)
  const sql = new pg.Client({ host: '127.0.0.1', port: databasePort, user: 'postgres', password: process.env.CANARY_PG_PASSWORD || 'postgres', database: 'postgres' })
  await sql.connect()
  try {
    // These names must be absent. Reuse requires a new disposable container;
    // never drop a database or replace a role supplied by someone else.
    await sql.query(`CREATE ROLE canary_owner LOGIN PASSWORD '${ownerPassword}' NOSUPERUSER CREATEDB CREATEROLE BYPASSRLS`)
    await sql.query('CREATE DATABASE rereply_canary_local OWNER canary_owner')
    await sql.query('SET ROLE canary_owner')
    await sql.query(`CREATE ROLE canary_app LOGIN PASSWORD '${runtimePassword}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`)
  } finally { await sql.end() }
  const origin = `http://127.0.0.1:${backendPort}`, stubOrigin = `http://127.0.0.1:${stubPort}`
  const databaseURL = role => `postgres://${role === 'owner' ? 'canary_owner:' + ownerPassword : 'canary_app:' + runtimePassword}@127.0.0.1:${databasePort}/rereply_canary_local?sslmode=disable`
  const config = `[app]
name = "ReReply"
environment = "test"
encryption_key = ${quote(encryptionKey)}
[server]
host = "127.0.0.1"
port = ${backendPort}
[redis]
url = "redis://127.0.0.1:${redisPort}/0"
[jwt]
secret = ${quote(jwtSecret)}
[storage]
type = "local"
local_path = ${quote(join(directory, 'uploads'))}
[whatsapp]
base_url = ${quote(stubOrigin)}
[ai]
qwen_base_url = "http://127.0.0.1:9"
[google_search_console]
auth_url = "http://127.0.0.1:9"
token_url = "http://127.0.0.1:9"
api_base_url = "http://127.0.0.1:9"
[meta_messenger_onboarding]
enabled = false
graph_base_url = "http://127.0.0.1:9"
[meta_instagram_onboarding]
enabled = false
authorization_base_url = "http://127.0.0.1:9"
token_base_url = "http://127.0.0.1:9"
graph_base_url = "http://127.0.0.1:9"
[threads_managed]
enabled = false
[default_admin]
email = "canary-bootstrap@example.test"
password = ${quote(adminPassword)}
full_name = "Synthetic Canary Bootstrap"
[database]
runtime_role = "canary_app"
rls_enabled = true
url = ${quote(databaseURL('runtime'))}
`
  write('server.toml', config)
  write('migrate.toml', config + `migration_url = ${quote(databaseURL('owner'))}\n`)
  write('private.json', JSON.stringify({ schema_version: 1, canary: { origin,
    namespace: 'rereply-local-' + randomBytes(6).toString('hex'), stub_origin: stubOrigin,
    stub_control_key: controlKey, stub_access_token: accessToken, stub_app_secret: appSecret, stub_app_id: '999100000000001',
    admin_email: 'canary-bootstrap@example.test', admin_password: adminPassword, config_path: join(directory, 'migrate.toml') },
    local: { database_url: databaseURL('runtime'), stub_port: stubPort } }, null, 2))
  console.log('Local private configuration created')
}

async function bootstrap() {
  assertPrivatePermissions(directory)
  const config = join(directory, 'migrate.toml')
  assertPrivateLocation(config, { mustExist: true })
  assertPrivatePermissions(config)
  // The bootstrap loader requires fixed, executable-relative profile files.
  // A go-run temporary executable cannot carry those siblings. Use the same
  // private disposable layout for local developers and GitHub-hosted canaries.
  const toolDirectory = mkdtempSync(join(directory, 'bootstrap-'))
  secureWindowsPath(toolDirectory, true)
  assertPrivatePermissions(toolDirectory)
  const executable = join(toolDirectory, process.platform === 'win32' ? 'bootstrap.exe' : 'bootstrap')
  const profileDirectory = join(toolDirectory, 'production-shape')
  mkdirSync(profileDirectory, { mode: 0o700 })
  copyFileSync(join(root, 'internal/dbcatalog/golden/production-v0.json'), join(profileDirectory, 'production-v0.json'))
  copyFileSync(join(root, 'internal/dbcatalog/shape/production-v0.sql'), join(profileDirectory, 'production-v0.sql'))
  const options = { cwd: root, env: childEnvironment(), stdio: 'inherit', windowsHide: true }
  execFileSync('go', ['build', '-mod=readonly', '-o', executable, './release/staging/bootstrap'], options)
  execFileSync(executable, ['-config', config], options)
}

async function start() {
  for (const path of [directory, privateFile, join(directory, 'server.toml')]) {
    assertPrivateLocation(path, { mustExist: true })
    assertPrivatePermissions(path)
  }
  const saved = JSON.parse(readFileSync(privateFile, 'utf8'))
  const values = saved.canary
  const localURL = (raw, protocols) => {
    const parsed = new URL(raw)
    if (!protocols.includes(parsed.protocol) || parsed.hostname !== '127.0.0.1') throw new Error('Saved local endpoints must remain on loopback')
    return parsed
  }
  localURL(values.origin, ['http:'])
  localURL(values.stub_origin, ['http:'])
  if (localURL(saved.local.database_url, ['postgres:']).pathname !== '/rereply_canary_local') throw new Error('Saved local database differs')
  const db = new pg.Client({ connectionString: saved.local.database_url })
  await db.connect()
  try {
    const result = await db.query('SELECT organization_id FROM users WHERE email = $1 AND is_super_admin = true', [values.admin_email])
    if (result.rows.length !== 1) throw new Error('Bootstrap must produce exactly one synthetic administrator')
    values.klinik_organization_id = result.rows[0].organization_id
  } finally { await db.end() }
  const serverConfig = readFileSync(join(directory, 'server.toml'), 'utf8')
  if (serverConfig.includes('[legacy_whatsapp_reply]')) throw new Error('Stack already configured; use its running instance')
  write('server.toml', serverConfig + `\n[legacy_whatsapp_reply]\nenabled = true\nallowed_organization_ids = ${quote(values.klinik_organization_id)}\n`)
  write('private.json', JSON.stringify(saved, null, 2))
  const launch = (name, args, env) => {
    const log = openSync(join(directory, name + '.log'), 'a', 0o600)
    const extension = process.platform === 'win32' ? '.exe' : ''
    const child = spawn(join(directory, name + extension), args, { cwd: root, detached: true, windowsHide: true,
      stdio: ['ignore', log, log], env: childEnvironment(process.env, env) })
    child.unref()
    return child.pid
  }
  const pids = [launch('graph-stub', [], { STUB_ENVIRONMENT: 'local', STUB_LISTEN_ADDR: `127.0.0.1:${saved.local.stub_port}`,
    STUB_CONTROL_KEY: values.stub_control_key, STUB_ACCESS_TOKENS: values.stub_access_token,
    STUB_APP_ID: values.stub_app_id, STUB_APP_SECRET: values.stub_app_secret, STUB_CALLBACK_ORIGIN: values.origin }),
    launch('rereply', ['server', '-config', join(directory, 'server.toml')], {})]
  write('processes.json', JSON.stringify(pids))
  for (let attempt = 0; attempt < 120; attempt++) {
    try { if ((await fetch(values.origin + '/ready', { signal: AbortSignal.timeout(1500), redirect: 'error' })).ok) { console.log('Local RLS backend and Graph stub started'); return } } catch {}
    await new Promise(resolve => setTimeout(resolve, 1000))
  }
  throw new Error('Local backend did not become ready; inspect private local logs')
}

const mode = process.argv[2]
try {
  if (mode === 'configure') await configure()
  else if (mode === 'bootstrap') await bootstrap()
  else if (mode === 'start') await start()
  else throw new Error('Expected configure, bootstrap or start')
} catch (error) { console.error(error.code ? 'Local setup failed: ' + error.code : error.message); process.exitCode = 1 }
