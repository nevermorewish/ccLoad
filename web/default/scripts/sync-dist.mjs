import { cp, mkdir, rm, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const repoWeb = path.resolve(root, '..')
const dist = path.join(root, 'dist')
const staticDir = path.join(repoWeb, 'static')
await rm(staticDir, { recursive: true, force: true })
await mkdir(staticDir, { recursive: true })
await cp(path.join(dist, 'static'), staticDir, { recursive: true })
await writeFile(path.join(repoWeb, 'index.html'), await readFile(path.join(dist, 'index.html')))
