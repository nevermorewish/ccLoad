import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { transform } from 'esbuild'

const repoRoot = dirname(fileURLToPath(import.meta.url))
const webRoot = join(repoRoot, 'web')
const bundleRoot = join(webRoot, 'assets', 'bundles')
const checkOnly = process.argv.includes('--check')

const pageFiles = [
  'channels.html',
  'index.html',
  'login.html',
  'monitor.html',
  'logs.html',
  'model-test.html',
  'settings.html',
  'stats.html',
  'tokens.html',
  'trend.html',
]

const scriptPattern = /<script\b([^>]*?)\bsrc="([^"]+)"([^>]*)><\/script>/gi
const stylesheetPattern = /<link\b([^>]*?)\brel="stylesheet"([^>]*?)\bhref="([^"]+)"([^>]*)>/gi
function toLocalPath(url) {
  const cleanURL = url.split('?', 1)[0]
  if (!cleanURL.startsWith('/web/')) return null
  return join(webRoot, cleanURL.slice('/web/'.length).replaceAll('/', '\\'))
}

async function readBundleManifest(pageName) {
  const manifestPath = join(bundleRoot, `${pageName}.json`)
  if (!existsSync(manifestPath)) return null
  return JSON.parse(await readFile(manifestPath, 'utf8'))
}

async function sourcesForPage(pageName, html) {
  const existing = await readBundleManifest(pageName)
  if (existing) return existing

  const scripts = [...html.matchAll(scriptPattern)]
    .map((match) => match[2])
    .filter((url) => url.includes('/assets/') && !url.includes('theme-init.js'))
  const stylesheets = [...html.matchAll(stylesheetPattern)]
    .map((match) => match[3])
    .filter((url) => url.startsWith('/web/'))
  return { scripts, stylesheets }
}

async function buildJavaScript(pageName, urls) {
  const chunks = []
  for (const url of urls) {
    const file = toLocalPath(url)
    if (!file) throw new Error(`Unsupported script URL in ${pageName}: ${url}`)
    chunks.push(`\n;/* ${url} */\n`, await readFile(file, 'utf8'))
  }
  const result = await transform(chunks.join(''), {
    loader: 'js',
    minify: true,
    legalComments: 'none',
    target: ['es2020'],
    sourcefile: `${pageName}.js`,
  })
  return result.code
}

async function buildStyles(pageName, urls) {
  const chunks = []
  for (const url of urls) {
    const file = toLocalPath(url)
    if (!file) throw new Error(`Unsupported stylesheet URL in ${pageName}: ${url}`)
    chunks.push(`\n/* ${url} */\n`, await readFile(file, 'utf8'))
  }
  const result = await transform(chunks.join(''), {
    loader: 'css',
    minify: true,
    legalComments: 'none',
    sourcefile: `${pageName}.css`,
  })
  return result.code
}

function replaceScripts(html, pageName) {
  if (html.includes('data-ccload-bundle')) return html
  const bundleURL = `/web/assets/bundles/${pageName}.js?v=__VERSION__`
  let inserted = false
  return html.replace(scriptPattern, (full, before, url, after) => {
    if (!url.includes('/assets/') || url.includes('theme-init.js')) return full
    if (inserted) return ''
    inserted = true
    return `<script defer data-ccload-bundle src="${bundleURL}"></script>`
  })
}

function replaceStyles(html, pageName, hasStyles) {
  if (!hasStyles) return html
  if (/<link\b[^>]*data-ccload-bundle/i.test(html)) {
    return html.replace(stylesheetPattern, (full) => full.includes('data-ccload-bundle') ? full : '')
  }
  const bundleURL = `/web/assets/bundles/${pageName}.css?v=__VERSION__`
  let replaced = false
  const output = html.replace(stylesheetPattern, (full, before, middle, url, after) => {
    if (!url.startsWith('/web/') || replaced) return full
    replaced = true
    return `<link rel="stylesheet" data-ccload-bundle href="${bundleURL}">`
  })
  return output.replace(/(?:<link rel="stylesheet" data-ccload-bundle[^>]*>\r?\n?)+/gi, `<link rel="stylesheet" data-ccload-bundle href="${bundleURL}">\n`)
}

function cleanWhitespaceOnlyLines(html) {
  return html.replace(/^[ \t]*\r?$/gm, '').replace(/(?:\r?\n){3,}/g, '\n\n')
}

async function main() {
  if (!checkOnly) await mkdir(bundleRoot, { recursive: true })
  for (const pageFile of pageFiles) {
    const pageName = pageFile.slice(0, -'.html'.length)
    const pagePath = join(webRoot, pageFile)
    const originalHTML = await readFile(pagePath, 'utf8')
    const manifest = await sourcesForPage(pageName, originalHTML)
    const js = await buildJavaScript(pageName, manifest.scripts)
    const css = await buildStyles(pageName, manifest.stylesheets)
    const jsPath = join(bundleRoot, `${pageName}.js`)
    const cssPath = join(bundleRoot, `${pageName}.css`)
    const manifestPath = join(bundleRoot, `${pageName}.json`)
    const outputHTML = cleanWhitespaceOnlyLines(replaceStyles(replaceScripts(originalHTML, pageName), pageName, manifest.stylesheets.length > 0))

    if (checkOnly) {
      for (const [file, expected] of [[jsPath, js], [cssPath, css]]) {
        const current = existsSync(file) ? await readFile(file, 'utf8') : ''
        if (current !== expected) throw new Error(`${relative(webRoot, file)} is stale; run npm run build`)
      }
      if (outputHTML !== originalHTML) throw new Error(`${pageFile} is not built; run npm run build`)
      continue
    }

    await writeFile(jsPath, js)
    await writeFile(cssPath, css)
    await writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`)
    await writeFile(pagePath, outputHTML)
    console.log(`${pageFile}: ${manifest.scripts.length} scripts -> ${Math.ceil(js.length / 1024)} KiB, ${manifest.stylesheets.length} stylesheets -> ${Math.ceil(css.length / 1024)} KiB`)
  }
}

main().catch((error) => {
  console.error(error)
  process.exitCode = 1
})
