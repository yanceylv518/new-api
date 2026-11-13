import { createHash } from 'node:crypto'
import {
  copyFile,
  mkdir,
  readFile,
  readdir,
  rename,
  stat,
  writeFile,
} from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.dirname(fileURLToPath(import.meta.url))
const releases = path.join(root, 'releases')
const output = path.join(root, 'public')
const catalogPath = path.join(root, 'catalog.json')
const keyPattern = /^[a-z0-9][a-z0-9_-]{0,29}$/
const versionPattern =
  /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/
const digest = (data) => createHash('sha256').update(data).digest('hex')

async function readCatalog() {
  try {
    const catalog = JSON.parse(await readFile(catalogPath, 'utf8'))
    if (catalog.indexVersion !== 1 || !Array.isArray(catalog.plugins))
      throw new Error('Unsupported marketplace catalog')
    return catalog
  } catch (error) {
    if (error.code === 'ENOENT')
      return { indexVersion: 1, name: 'PinduCloud', plugins: [] }
    throw error
  }
}

function releaseFile(relative) {
  const target = path.resolve(releases, '.' + relative)
  if (!relative.startsWith('/plugins/') && !relative.startsWith('/licenses/'))
    throw new Error('Invalid release path')
  if (!target.startsWith(releases + path.sep))
    throw new Error('Release path escapes the catalog')
  return target
}

async function writeSnapshot(relative, data) {
  const target = releaseFile(relative)
  try {
    const current = await readFile(target)
    if (!current.equals(data))
      throw new Error(
        'Published files are immutable; bump the plugin version: ' + relative
      )
  } catch (error) {
    if (error.code !== 'ENOENT') throw error
    await mkdir(path.dirname(target), { recursive: true })
    await writeFile(target, data, { flag: 'wx' })
  }
}

async function syncPlugins(catalog) {
  const sourceRoot = path.resolve(root, '../plugins/tasks')
  const directories = await readdir(sourceRoot, { withFileTypes: true })
  const plans = []
  for (const directory of directories
    .filter((item) => item.isDirectory())
    .sort((a, b) => a.name.localeCompare(b.name))) {
    const source = await readFile(
      path.join(sourceRoot, directory.name, 'plugin.js')
    )
    if (source.length > 8 * 1024 * 1024)
      throw new Error('Plugin source exceeds the gateway limit')
    // 这里只打包本仓库受信任的源码；外部市场插件必须走网关的审核与编译流程。
    const { meta } = await import(
      'data:text/javascript;base64,' + source.toString('base64')
    )
    if (
      !meta ||
      meta.apiVersion !== 1 ||
      meta.key !== directory.name ||
      !keyPattern.test(meta.key) ||
      !versionPattern.test(meta.version)
    ) {
      throw new Error('Invalid plugin manifest: ' + directory.name)
    }
    const previous = catalog.plugins.find((item) => item.key === meta.key)
    const version = {
      version: meta.version,
      kind: 'task',
      minApiVersion: meta.apiVersion,
      path: '/plugins/' + meta.key + '/' + meta.version + '/plugin.js',
      sha256: digest(source),
      allowedHosts: meta.allowedHosts || [],
      auth:
        typeof meta.auth === 'object' ? meta.auth.type : meta.auth || 'api_key',
    }
    if (meta.baseUrl) version.baseUrl = meta.baseUrl
    const oldVersion = previous?.versions.find(
      (item) => item.version === meta.version
    )
    if (oldVersion && oldVersion.sha256 !== version.sha256)
      throw new Error(meta.key + ': source changed without a version bump')
    const plugin = {
      key: meta.key,
      name: meta.name,
      author: meta.author,
      description: meta.description,
      models: meta.models,
      latest: meta.version,
    }
    for (const key of [
      'icon',
      'channelTypes',
      'protocols',
      'sortPriority',
      'website',
    ])
      if (meta[key] !== undefined) plugin[key] = meta[key]
    plugin.versions = [
      version,
      ...(previous?.versions || []).filter(
        (item) => item.version !== meta.version
      ),
    ]
    const files = [{ relative: version.path, data: source }]
    for (const name of ['icon.svg', 'icon.png']) {
      try {
        const icon = await readFile(path.join(sourceRoot, directory.name, name))
        if (icon.length > 512 * 1024)
          throw new Error('Plugin icon exceeds the gateway limit')
        plugin.iconFile = {
          path: '/plugins/' + meta.key + '/' + meta.version + '/' + name,
          sha256: digest(icon),
        }
        files.push({ relative: plugin.iconFile.path, data: icon })
        break
      } catch (error) {
        if (error.code !== 'ENOENT') throw error
      }
    }
    plans.push({ plugin, files })
  }
  for (const plan of plans)
    for (const file of plan.files) await writeSnapshot(file.relative, file.data)
  for (const name of ['LICENSE', 'NOTICE', 'THIRD-PARTY-LICENSES.md']) {
    const target = releaseFile('/licenses/' + name)
    await mkdir(path.dirname(target), { recursive: true })
    await copyFile(path.resolve(root, '..', name), target)
  }
  const next = {
    indexVersion: 1,
    name: catalog.name,
    plugins: plans.map((plan) => plan.plugin),
  }
  const temporary = catalogPath + '.tmp'
  await writeFile(temporary, JSON.stringify(next, null, 2) + '\n')
  await rename(temporary, catalogPath)
  return next
}

async function build(catalog) {
  if (!catalog.plugins.length)
    throw new Error(
      'Catalog is empty; run node build.mjs --sync from the source checkout first'
    )
  const copies = []
  for (const plugin of catalog.plugins) {
    if (
      !keyPattern.test(plugin.key) ||
      !plugin.versions.some((item) => item.version === plugin.latest)
    )
      throw new Error('Invalid catalog plugin')
    for (const version of plugin.versions) {
      if (!versionPattern.test(version.version))
        throw new Error('Invalid catalog version')
      const source = await readFile(releaseFile(version.path))
      if (digest(source) !== version.sha256)
        throw new Error(
          'Plugin integrity mismatch: ' + plugin.key + '@' + version.version
        )
      copies.push({ relative: version.path, source: releaseFile(version.path) })
    }
    if (plugin.iconFile) {
      const source = await readFile(releaseFile(plugin.iconFile.path))
      if (digest(source) !== plugin.iconFile.sha256)
        throw new Error('Icon integrity mismatch: ' + plugin.key)
      copies.push({
        relative: plugin.iconFile.path,
        source: releaseFile(plugin.iconFile.path),
      })
    }
  }
  for (const name of ['LICENSE', 'NOTICE', 'THIRD-PARTY-LICENSES.md']) {
    const relative = '/licenses/' + name
    await stat(releaseFile(relative))
    copies.push({ relative, source: releaseFile(relative) })
  }
  await mkdir(output, { recursive: true })
  for (const copy of copies) {
    const destination = path.join(output, copy.relative.slice(1))
    await mkdir(path.dirname(destination), { recursive: true })
    await copyFile(copy.source, destination)
  }
  await writeFile(
    path.join(output, 'index.json'),
    JSON.stringify(catalog, null, 2) + '\n'
  )
  process.stdout.write(
    JSON.stringify({
      name: catalog.name,
      plugins: catalog.plugins.length,
      versions: catalog.plugins.reduce(
        (sum, item) => sum + item.versions.length,
        0
      ),
      output,
    }) + '\n'
  )
}

const args = process.argv.slice(2)
if (args.length > 1 || (args.length === 1 && args[0] !== '--sync'))
  throw new Error('Usage: node build.mjs [--sync]')
let catalog = await readCatalog()
if (args[0] === '--sync') catalog = await syncPlugins(catalog)
await build(catalog)
