import { randomBytes } from 'node:crypto'
import { closeSync, existsSync, fsyncSync, lstatSync, mkdtempSync, openSync, realpathSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { dirname, isAbsolute, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { execFileSync } from 'node:child_process'

export const checkoutRoot = fileURLToPath(new URL('../../../', import.meta.url))
const inside = (parent, target) => { const value = relative(parent, target); return !value || (!value.startsWith('..') && !isAbsolute(value)) }
export const forbiddenAmbient = key => /^(WHATOMATE_|STUB_|META_RELAY_|GMAIL_RELAY_)/i.test(key)
export function refuseAmbient(env = process.env) {
  if (Object.keys(env).some(forbiddenAmbient)) throw new Error('Ambient application or relay configuration is forbidden for local canaries')
}
export function childEnvironment(env = process.env, extra = {}) {
  // Defense in depth even when a caller has already refused ambient overrides.
  return { ...Object.fromEntries(Object.entries(env).filter(([key]) => !forbiddenAmbient(key))), ...extra }
}

export function assertPrivateLocation(path, { mustExist = false } = {}) {
  if (!path || !isAbsolute(path)) throw new Error('Private output path must be absolute')
  const target = resolve(path)
  if (inside(realpathSync(checkoutRoot), target)) throw new Error('Private output must be outside the checkout')
  let current = target
  for (;;) {
    let info
    try { info = lstatSync(current) } catch (error) { if (error.code !== 'ENOENT' && error.code !== 'ENOTDIR') throw error }
    if (info) {
      if (info.isSymbolicLink()) throw new Error('Symlinks are forbidden in private output paths')
      if (existsSync(join(current, '.git'))) throw new Error('Private output must not be inside a Git checkout')
      const actual = realpathSync(current)
      if (inside(realpathSync(checkoutRoot), actual)) throw new Error('Private output resolves inside the checkout')
    } else if (current === target && mustExist) throw new Error('Private file does not exist')
    const parent = dirname(current)
    if (parent === current) break
    current = parent
  }
  return target
}

function powershell(script, path) {
  return execFileSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
    windowsHide: true, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...childEnvironment(), CANARY_ACL_TARGET: path },
  })
}
export function secureWindowsPath(path, directory = false) {
  if (process.platform !== 'win32') return
  try {
    const kind = directory ? 'Directory' : 'File'
    const flags = directory ? 'ContainerInherit, ObjectInherit' : 'None'
    powershell(`$ErrorActionPreference='Stop'; $me=[System.Security.Principal.WindowsIdentity]::GetCurrent().User; $acl=New-Object System.Security.AccessControl.${kind}Security; $acl.SetOwner($me); $acl.SetAccessRuleProtection($true,$false); $rule=New-Object System.Security.AccessControl.FileSystemAccessRule($me,'FullControl','${flags}','None','Allow'); $acl.AddAccessRule($rule); [System.IO.${kind}]::SetAccessControl($env:CANARY_ACL_TARGET,$acl)`, path)
  } catch { throw new Error('Cannot secure private-file ACL') }
}
export function assertPrivatePermissions(path) {
  if (process.platform !== 'win32') {
    if (lstatSync(path).mode & 0o077) throw new Error('Private files and directories require owner-only permissions')
    return
  }
  try {
    powershell(`$ErrorActionPreference='Stop'; $me=[System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value; if([System.IO.Directory]::Exists($env:CANARY_ACL_TARGET)){$acl=[System.IO.Directory]::GetAccessControl($env:CANARY_ACL_TARGET)}else{$acl=[System.IO.File]::GetAccessControl($env:CANARY_ACL_TARGET)}; if($acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value -ne $me){exit 1}; $ownerAllowed=$false; foreach($rule in $acl.Access){ if($rule.AccessControlType -eq 'Allow'){ $sid=$rule.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value; if($sid -ne $me){exit 1}; $ownerAllowed=$true } }; if(-not $ownerAllowed){exit 1}; exit 0`, path)
  } catch { throw new Error('Private file ACL permits another principal') }
}

export function atomicWritePrivate(path, contents) {
  const target = assertPrivateOutput(path)
  const temporary = join(dirname(target), '.canary-' + randomBytes(12).toString('hex') + '.tmp')
  const descriptor = openSync(temporary, 'wx', 0o600)
  try { writeFileSync(descriptor, contents); fsyncSync(descriptor) } finally { closeSync(descriptor) }
  try {
    secureWindowsPath(temporary)
    renameSync(temporary, target)
  } finally { if (existsSync(temporary)) unlinkSync(temporary) }
}

export function assertPrivateOutput(path) {
  const target = assertPrivateLocation(path)
  assertPrivatePermissions(dirname(target))
  if (existsSync(target)) {
    if (!lstatSync(target).isFile()) throw new Error('Private output must be a regular file')
    assertPrivatePermissions(target)
  }
  return target
}

export function validateReportPath(privateFile, reportFile) {
  const state = assertPrivateLocation(privateFile, { mustExist: true })
  const report = assertPrivateOutput(reportFile)
  const canonical = value => process.platform === 'win32' ? value.toLowerCase() : value
  if (canonical(state) === canonical(report)) throw new Error('Report must not replace the private setup file')
  if (!inside(dirname(state), report)) throw new Error('Report must stay beside the private setup file')
  if (existsSync(report)) {
    const original = lstatSync(state), output = lstatSync(report)
    if (original.ino && original.ino === output.ino && original.dev === output.dev) throw new Error('Report aliases the private setup file')
  }
  return report
}

export function createPrivateResultsDirectory(privateFile) {
  const state = assertPrivateLocation(privateFile, { mustExist: true })
  assertPrivatePermissions(dirname(state))
  const directory = mkdtempSync(join(dirname(state), 'playwright-results-'))
  secureWindowsPath(directory, true)
  assertPrivatePermissions(directory)
  return directory
}
