import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

const skipped = new Set([".git", ".gitdb", "node_modules", "data", "dist", "playwright-report", "test-results"]);
const checkedExtensions = new Set([
  ".css",
  ".go",
  ".html",
  ".js",
  ".json",
  ".md",
  ".mod",
  ".sum",
  ".ts",
  ".tsx",
  ".yaml",
  ".yml"
]);

function walk(dir, result = []) {
  for (const entry of readdirSync(dir)) {
    if (skipped.has(entry)) {
      continue;
    }
    const path = join(dir, entry);
    const stat = statSync(path);
    if (stat.isDirectory()) {
      walk(path, result);
      continue;
    }
    const ext = entry.slice(entry.lastIndexOf("."));
    if (checkedExtensions.has(ext) || entry === "Dockerfile" || entry === "Makefile") {
      result.push(path);
    }
  }
  return result;
}

const files = walk(".");

let failed = false;

for (const file of files) {
  const text = readFileSync(join(process.cwd(), file), "utf8");
  text.split("\n").forEach((line, index) => {
    if (/\s$/.test(line)) {
      console.error(`${file}:${index + 1}: trailing whitespace`);
      failed = true;
    }
  });
}

if (failed) {
  process.exit(1);
}
