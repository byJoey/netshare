import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const lock = JSON.parse(
  fs.readFileSync(path.join(root, "package-lock.json"), "utf8"),
);
const sections = [
  "# NetShare 前端运行时依赖许可\n\n由锁文件与已安装 npm 包生成，保留各包许可和归属原文。\n",
];
const missing = [];
let count = 0;
for (const [relative, entry] of Object.entries(lock.packages)) {
  if (!relative || entry.dev) continue;
  const directory = path.join(root, relative);
  if (!fs.existsSync(directory)) {
    missing.push(relative);
    continue;
  }
  const manifest = JSON.parse(
    fs.readFileSync(path.join(directory, "package.json"), "utf8"),
  );
  const files = fs
    .readdirSync(directory)
    .filter(
      (name) =>
        /^(licen[sc]e|copying|notice)(\.|$|-)/i.test(name) &&
        fs.statSync(path.join(directory, name)).isFile(),
    )
    .sort();
  if (!files.length) {
    const fallback = path.join(
      root,
      "licenses",
      `LICENSE.${manifest.name.replaceAll("/", "_")}`,
    );
    if (fs.existsSync(fallback)) {
      sections.push(
        `\n${"=".repeat(72)}\n${manifest.name}@${manifest.version}\n\n${fs.readFileSync(fallback, "utf8").trim()}\n`,
      );
      count++;
      continue;
    }
    missing.push(`${manifest.name}@${manifest.version}`);
    continue;
  }
  sections.push(
    `\n${"=".repeat(72)}\n${manifest.name}@${manifest.version}\n许可：${typeof manifest.license === "string" ? manifest.license : JSON.stringify(manifest.license || "见下文")}\n`,
  );
  for (const name of files)
    sections.push(
      `\n--- ${name} ---\n\n${fs.readFileSync(path.join(directory, name), "utf8").trim()}\n`,
    );
  count++;
}
if (missing.length)
  throw new Error(`缺少运行时依赖许可原文：${missing.join("、")}`);
fs.mkdirSync(path.join(root, "licenses"), { recursive: true });
fs.writeFileSync(
  path.join(root, "licenses/LICENSES.dependencies.txt"),
  sections.join("\n"),
);
process.stdout.write(`已收集 ${count} 个运行时依赖的许可原文。\n`);
