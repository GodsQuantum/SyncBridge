import adapter from '@sveltejs/adapter-static';
import { createHash } from 'node:crypto';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const hash = createHash('sha256');
function hashPath(path) {
  const stat = statSync(path);
  if (stat.isDirectory()) {
    for (const name of readdirSync(path).sort()) hashPath(join(path, name));
    return;
  }
  hash.update(relative(root, path));
  hash.update('\0');
  hash.update(readFileSync(path));
  hash.update('\0');
}
for (const name of ['src', 'package.json', 'package-lock.json', 'vite.config.ts', 'tsconfig.json']) {
  hashPath(join(root, name));
}
const versionName = hash.digest('hex').slice(0, 16);

/** @type {import('@sveltejs/kit').Config} */
const config = {
  kit: {
    version: { name: versionName, pollInterval: 0 },
    adapter: adapter({
      pages: '../cmd/syncbridge/web',
      assets: '../cmd/syncbridge/web',
      precompress: false,
      strict: true
    })
  }
};

export default config;
