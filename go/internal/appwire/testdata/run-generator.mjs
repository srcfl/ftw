/* Runs generate-vectors.ts against a checkout of srcfl/ftw-webapp.
 *
 *   node go/internal/appwire/testdata/run-generator.mjs ../ftw-webapp
 *
 * It boots the app's own Vite server so the generator sees the same aliases,
 * the same TypeScript settings and the same crypto packages the app ships.
 * Loading Vite by absolute path is what lets this script live in the Go
 * repository, beside the vectors it produces, instead of in the app.
 */

import { resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const appRoot = resolve(process.argv[2] ?? '../ftw-webapp')
const vite = await import(pathToFileURL(resolve(appRoot, 'node_modules/vite/dist/node/index.js')).href)

const server = await vite.createServer({
  root: appRoot,
  configFile: resolve(appRoot, 'vite.config.ts'),
  server: { middlewareMode: true },
  appType: 'custom',
  logLevel: 'warn',
})

try {
  const generator = fileURLToPath(new URL('./generate-vectors.ts', import.meta.url))
  const mod = await server.ssrLoadModule(generator)
  const target = fileURLToPath(new URL('./webapp-vectors.json', import.meta.url))
  await mod.generate(target)
  console.log(`wrote ${target}`)
} finally {
  await server.close()
}
