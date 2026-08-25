#!/usr/bin/env node
// Parse every ```mermaid fence in the documentation with Mermaid's real
// grammar, not a structural approximation.
//
// The Go contract suite (TestMermaidDiagramsAreWellFormed) checks edge
// operators, which is the class it was written for. It cannot see what the
// grammar itself rejects: a participant named `Loop` lexes as the reserved
// `loop` keyword, and a raw `;` inside an escape sequence is a statement
// separator. Both shipped in this repository, both rendered as GitHub parse
// errors on the project's front door, and neither tripped the structural
// check. This is the authoritative gate: hand every fenced block to
// mermaid.parse and fail on anything the renderer would refuse.
//
//   node scripts/check-mermaid.mjs            # every tracked *.md file
//   node scripts/check-mermaid.mjs FILE...    # named files, for debugging
//
// Exit status is 0 only when every block parsed and, in tracked-file mode,
// the block floor held — a scanner that stops finding blocks must fail
// loudly rather than pass vacuously.

import fs from "fs";
import path from "path";
import { execFileSync } from "child_process";
import { JSDOM } from "jsdom";

const MIN_BLOCKS = 20; // mirrors the floor in TestMermaidDiagramsAreWellFormed

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), "..");

function trackedMarkdownFiles() {
  const out = execFileSync("git", ["ls-files", "*.md"], { cwd: repoRoot, encoding: "utf8" });
  return out.split("\n").filter(Boolean).filter((f) => fs.existsSync(path.join(repoRoot, f)));
}

// Line-scan instead of one regex so indented fences and CRLF files are handled
// exactly as CommonMark would, and every block carries its true start line.
function extractMermaidBlocks(text) {
  const lines = text.split(/\r?\n/);
  const blocks = [];
  let i = 0;
  while (i < lines.length) {
    if (/^\s*```mermaid\s*$/.test(lines[i])) {
      const startLine = i + 1;
      const body = [];
      i++;
      while (i < lines.length && !/^\s*```\s*$/.test(lines[i])) {
        body.push(lines[i]);
        i++;
      }
      if (i >= lines.length) {
        return { error: `${startLine}: fence opens but never closes` };
      }
      blocks.push({ startLine, body: body.join("\n") + "\n" });
    }
    i++;
  }
  return { blocks };
}

async function main() {
  // Mermaid's parser needs a DOM. jsdom provides the smallest one that works;
  // these globals mirror what a browser gives it.
  const dom = new JSDOM("<!DOCTYPE html><html><body></body></html>");
  global.window = dom.window;
  global.document = dom.window.document;
  Object.defineProperty(global, "navigator", { value: dom.window.navigator, configurable: true });
  global.DOMParser = dom.window.DOMParser;
  global.XMLSerializer = dom.window.XMLSerializer;

  const { default: mermaid } = await import("mermaid");
  mermaid.initialize({ startOnLoad: false });

  let version = "unknown";
  try {
    version = JSON.parse(
      fs.readFileSync(new URL("../node_modules/mermaid/package.json", import.meta.url), "utf8"),
    ).version;
  } catch {
    // The version is context for drift against GitHub's renderer, not a gate.
  }

  const explicit = process.argv.slice(2);
  const gitMode = explicit.length === 0;
  const files = gitMode ? trackedMarkdownFiles() : explicit;

  if (files.length === 0) {
    console.error("FAIL no markdown files to scan");
    process.exit(1);
  }

  let total = 0;
  let failed = 0;
  for (const file of files) {
    const abs = gitMode ? path.join(repoRoot, file) : path.resolve(process.cwd(), file);
    const text = fs.readFileSync(abs, "utf8");
    const { blocks, error } = extractMermaidBlocks(text);
    if (error) {
      failed++;
      total++;
      console.error(`FAIL ${file}:${error}`);
      continue;
    }
    for (const [k, blk] of blocks.entries()) {
      total++;
      try {
        await mermaid.parse(blk.body);
      } catch (e) {
        failed++;
        const msg = String(e.message).split("\n").slice(0, 3).join(" | ").trim();
        const diagramLine = Number((e.message.match(/line (\d+)/) || [])[1]);
        const at = Number.isFinite(diagramLine)
          ? `block at line ${blk.startLine}, diagram line ${diagramLine} (file line ${blk.startLine + diagramLine})`
          : `block at line ${blk.startLine}`;
        console.error(`FAIL ${file}:${at} [#${k + 1}]: ${msg}`);
      }
    }
  }

  if (failed > 0) {
    console.error(`FAIL ${failed} of ${total} mermaid blocks did not parse (mermaid@${version})`);
    process.exit(1);
  }
  if (gitMode && total < MIN_BLOCKS) {
    console.error(`FAIL only ${total} mermaid blocks found across ${files.length} files; the scan is not looking at the diagrams`);
    process.exit(1);
  }
  console.log(`OK ${total} mermaid blocks in ${files.length} files parsed with mermaid@${version}`);
}

main().catch((e) => {
  console.error(`FAIL ${e?.stack || e}`);
  process.exit(1);
});
