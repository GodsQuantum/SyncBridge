import { readFile, readdir, writeFile } from "node:fs/promises";
import { join } from "node:path";

const root = new URL("../../cmd/syncbridge/web/", import.meta.url);

async function walk(dir) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) await walk(path);
    else if (entry.name.endsWith(".html")) {
      const input = await readFile(path, "utf8");
      const output = input.replace(/[ \t]+$/gm, "");
      if (output !== input) await writeFile(path, output);
    }
  }
}

await walk(root.pathname);
