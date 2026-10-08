#!/usr/bin/env node
/**
 * Release gate for the manvi tag workflow.
 *
 * Notes ship from docs/releases/<tag>.md via --notes-file. A body stuffed
 * into an environment variable dies at GitHub's 48 KB cap after the matrix
 * has already built. The curated file also has to stay under 100000
 * characters, inside the 125000-character release-body limit.
 *
 * The Go build depends on replace directives that point at sibling
 * checkouts. Those directories are absent on a runner, so verify.yml and
 * release.yml both have to fetch the pins in scripts/module-pins.txt
 * before go runs.
 *
 * Exit codes: 0 holds · 1 a check failed · 2 the check could not run.
 */
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import {
  closeSync,
  existsSync,
  lstatSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
  realpathSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

export const NOTES_LIMIT = 100_000;

export const TARGETS = Object.freeze([
  "darwin-amd64",
  "darwin-arm64",
  "linux-amd64",
  "linux-arm64",
]);

const ZIP_LOCAL = Buffer.from([0x50, 0x4b, 0x03, 0x04]);

/**
 * @param {string} tag
 * @returns {{ ok: true, tag: string, version: string } | { ok: false, reason: string }}
 */
export function parseTag(tag) {
  const match = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.exec(tag);
  if (!match) {
    return {
      ok: false,
      reason: `tag ${JSON.stringify(tag)} must be v<major>.<minor>.<patch> with no suffix`,
    };
  }
  return { ok: true, tag, version: `${match[1]}.${match[2]}.${match[3]}` };
}

/**
 * @param {string} tag
 * @returns {string[]}
 */
export function expectedBinaries(tag) {
  return TARGETS.map((target) => `manvi-${tag}-${target}`);
}

/**
 * The tag that will be pushed has to name this commit. CI checks out the
 * tag, so at that commit the notes are internally consistent and the
 * discrepancy is invisible. Measured on the sibling DevCouncil release:
 * v0.2.2 named a commit whose notes were 277 lines against 653 at HEAD.
 *
 * @param {{ tag: string, tagSha: string | null, headSha: string }} input
 * @returns {{ ok: true } | { ok: false, reason: string }}
 */
export function tagNamesHead({ tag, tagSha, headSha }) {
  if (!tagSha) {
    return { ok: false, reason: `tag ${tag} does not exist; the release would not publish this commit` };
  }
  if (tagSha === headSha) return { ok: true };
  const short = (/** @type {string} */ sha) => sha.slice(0, 12);
  return {
    ok: false,
    reason:
      `tag ${tag} names ${short(tagSha)} but HEAD is ${short(headSha)}; ` +
      `the release would publish docs/releases/${tag}.md as it was at ${short(tagSha)}`,
  };
}

/**
 * @param {string} notes
 * @param {string} tag
 * @param {string} label
 * @returns {string[]}
 */
export function notesProblems(notes, tag, label) {
  /** @type {string[]} */
  const errors = [];
  if (!notes.trim()) {
    errors.push(`release notes are empty: ${label}`);
    return errors;
  }
  if (!notes.includes(tag)) {
    errors.push(`release notes do not mention ${tag}: ${label}`);
  }
  if (notes.length > NOTES_LIMIT) {
    errors.push(
      `release notes are ${notes.length} characters; the cap is ${NOTES_LIMIT} so the body stays inside GitHub's 125000-character limit: ${label}`,
    );
  }
  return errors;
}

/**
 * @param {string} dir
 * @param {string} tag
 * @returns {string[]}
 */
export function assetProblems(dir, tag) {
  /** @type {string[]} */
  const errors = [];
  const expected = expectedBinaries(tag);
  if (!existsSync(dir)) {
    return expected.map((name) => `missing binary: ${name}`);
  }
  for (const name of expected) {
    const full = path.join(dir, name);
    if (!existsSync(full)) {
      errors.push(`missing binary: ${name}`);
      continue;
    }
    const info = statSync(full);
    if (!info.isFile()) {
      errors.push(`not a file: ${name}`);
      continue;
    }
    if (info.size === 0) {
      errors.push(`empty binary: ${name}`);
      continue;
    }
    const handle = openSync(full, "r");
    try {
      const magic = Buffer.alloc(4);
      const n = readSync(handle, magic, 0, 4, 0);
      if (n >= 4 && magic.subarray(0, 4).equals(ZIP_LOCAL)) {
        errors.push(`${name} is still a zip archive; download-artifact did not unpack it`);
      }
    } finally {
      closeSync(handle);
    }
  }
  for (const entry of readdirSync(dir)) {
    if (entry.startsWith("manvi-") && !expected.includes(entry)) {
      errors.push(`unexpected release file: ${entry}`);
    }
  }
  return errors;
}

/**
 * shasum -a 256 format: hex, two spaces, basename. `shasum -c` reads it
 * from the asset directory.
 *
 * @param {string} dir
 * @param {string} tag
 */
export function writeChecksums(dir, tag) {
  const lines = expectedBinaries(tag).map((name) => {
    const hash = createHash("sha256").update(readFileSync(path.join(dir, name))).digest("hex");
    return `${hash}  ${name}`;
  });
  const body = `${lines.join("\n")}\n`;
  writeFileSync(path.join(dir, "SHA256SUMS"), body);
  return body;
}

/**
 * @param {string} pins
 * @returns {{ name: string, url: string, sha: string }[]}
 */
export function parsePins(pins) {
  /** @type {{ name: string, url: string, sha: string }[]} */
  const rows = [];
  for (const raw of pins.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const parts = raw.split("\t");
    if (parts.length < 3) {
      throw new Error(`module pin line is missing columns: ${line}`);
    }
    const [name, url, sha] = parts;
    if (!/^[A-Za-z0-9._-]+$/.test(name)) throw new Error(`bad module name: ${name}`);
    if (!/^https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+\.git$/.test(url)) {
      throw new Error(`bad module url: ${url}`);
    }
    if (!/^[0-9a-f]{40}$/.test(sha)) throw new Error(`bad module sha for ${name}: ${sha}`);
    rows.push({ name, url, sha });
  }
  if (rows.length === 0) throw new Error("module pin file listed nothing");
  return rows;
}

/**
 * @param {{
 *   releaseYaml: string,
 *   verifyYaml: string,
 *   publishScript: string,
 *   buildScript: string,
 * }} sources
 * @returns {string[]}
 */
export function workflowProblems(sources) {
  /** @type {string[]} */
  const errors = [];
  const { releaseYaml, verifyYaml, publishScript, buildScript } = sources;
  if (!publishScript.includes("--notes-file") || !publishScript.includes("docs/releases/")) {
    errors.push("release publish does not read docs/releases/ via --notes-file");
  }
  for (const cmd of ["gh release view", "gh release edit", "gh release upload", "gh release create"]) {
    if (!publishScript.includes(cmd)) errors.push(`release publish is missing ${cmd}`);
  }
  if (publishScript.includes("GITHUB_ENV") || publishScript.includes("ANNOUNCEMENT_BODY")) {
    errors.push("release publish puts notes into an environment value; GitHub caps those at 48 KB");
  }
  if (/generate_release_notes:\s*true/.test(releaseYaml)) {
    errors.push("release.yml generates notes; the curated file would not be the release body");
  }
  if (!releaseYaml.includes("scripts/fetch-modules.sh")) {
    errors.push("release.yml does not fetch module replacements before the build");
  }
  if (!releaseYaml.includes("scripts/release-build.sh")) {
    errors.push("release.yml does not build through scripts/release-build.sh");
  }
  if (!releaseYaml.includes("scripts/release-publish.sh")) {
    errors.push("release.yml does not publish through scripts/release-publish.sh");
  }
  if (!releaseYaml.includes("cancel-in-progress: false")) {
    errors.push("release.yml cancels an in-progress publish");
  }
  const runBlocks = releaseYaml.split(/\n[ \t]*run:/).slice(1);
  for (const block of runBlocks) {
    const body = block.split(/\n[ \t]*- /)[0];
    if (body.includes("${{")) {
      errors.push("release.yml interpolates an expression into a run script");
      break;
    }
  }
  const verifyParts = verifyYaml.split("./verify.sh");
  if (verifyParts.length < 2) {
    errors.push("verify.yml does not run verify.sh");
  } else if (!verifyParts.slice(0, -1).every((part) => part.includes("scripts/fetch-modules.sh"))) {
    errors.push("verify.yml runs verify.sh without fetching module replacements first");
  }
  if (!buildScript.includes("stampedVersion")) {
    errors.push("release build does not stamp main.stampedVersion");
  }
  return errors;
}

/**
 * @param {string} root
 * @returns {string[]}
 */
export function notesFileProblems(root) {
  /** @type {string[]} */
  const errors = [];
  const dir = path.join(root, "docs", "releases");
  if (!existsSync(dir)) {
    return ["missing docs/releases"];
  }
  const files = readdirSync(dir).filter((name) => /^v\d+\.\d+\.\d+\.md$/.test(name)).sort();
  if (files.length === 0) errors.push("docs/releases has no vX.Y.Z notes");
  for (const name of files) {
    const tag = name.replace(/\.md$/, "");
    const parsed = parseTag(tag);
    if (!parsed.ok) {
      errors.push(parsed.reason);
      continue;
    }
    const full = path.join(dir, name);
    errors.push(...notesProblems(readFileSync(full, "utf8"), tag, full));
  }
  return errors;
}

/**
 * @param {string} root
 * @param {boolean} requireCheckouts
 * @returns {string[]}
 */
export function pinProblems(root, requireCheckouts) {
  /** @type {string[]} */
  const errors = [];
  const file = path.join(root, "scripts", "module-pins.txt");
  if (!existsSync(file)) return ["missing scripts/module-pins.txt"];
  let rows;
  try {
    rows = parsePins(readFileSync(file, "utf8"));
  } catch (err) {
    return [err instanceof Error ? err.message : String(err)];
  }
  const names = new Set(rows.map((row) => row.name));
  for (const required of ["DevCouncil", "gusset"]) {
    if (!names.has(required)) errors.push(`module pins do not name ${required}`);
  }
  if (!requireCheckouts) return errors;
  for (const row of rows) {
    const dest = path.join(root, "..", row.name);
    if (!existsSync(path.join(dest, ".git"))) {
      errors.push(`module ${row.name} is not checked out at ${dest}`);
      continue;
    }
    const have = execFileSync("git", ["-C", dest, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    if (have !== row.sha) {
      errors.push(
        `${row.name} is at ${have.slice(0, 12)} but scripts/module-pins.txt pins ${row.sha.slice(0, 12)}. The release builds the pin.`,
      );
    }
  }
  return errors;
}

/**
 * Strips a TOML comment, leaving a `#` inside a string alone.
 * @param {string} line
 */
function stripTomlComment(line) {
  let quote = "";
  for (let i = 0; i < line.length; i += 1) {
    const ch = line[i];
    if (quote) {
      if (ch === "\\" && quote === '"') i += 1;
      else if (ch === quote) quote = "";
    } else if (ch === '"' || ch === "'") quote = ch;
    else if (ch === "#") return line.slice(0, i);
  }
  return line;
}

/**
 * A value with the whitespace outside its strings removed, so two spellings of
 * one spec compare equal and any other difference does not.
 * @param {string} value
 */
export function normalizeTomlValue(value) {
  let out = "";
  let quote = "";
  for (let i = 0; i < value.length; i += 1) {
    const ch = value[i];
    if (quote) {
      out += ch;
      if (ch === "\\" && quote === '"') out += value[++i] ?? "";
      else if (ch === quote) quote = "";
    } else if (ch === '"' || ch === "'") {
      quote = ch;
      out += ch;
    } else if (!/\s/.test(ch)) out += ch;
  }
  return out;
}

/**
 * The tables of a Cargo manifest, as `table -> key -> normalized value`.
 *
 * Only what Cargo manifests here use: `[table]` headers and `key = value`
 * lines, a value that opens a bracket or brace running until it closes. An
 * array-of-tables header (`[[bin]]`) is kept as its own table name, and a
 * line this cannot read is refused rather than skipped, because a key it
 * skipped would be a key the mirror check never compared.
 * @param {string} text
 * @param {string} label
 * @returns {Map<string, Map<string, string>>}
 */
export function parseManifest(text, label) {
  /** @type {Map<string, Map<string, string>>} */
  const tables = new Map([["", new Map()]]);
  let current = "";
  let pendingKey = "";
  let pending = "";
  let depth = 0;
  const lines = text.split(/\r?\n/);
  for (let index = 0; index < lines.length; index += 1) {
    const line = stripTomlComment(lines[index]).trim();
    if (depth > 0) {
      pending += ` ${line}`;
      depth += balance(line);
      if (depth <= 0) {
        tables.get(current)?.set(pendingKey, normalizeTomlValue(pending));
        depth = 0;
      }
      continue;
    }
    if (!line) continue;
    const header = /^\[\[?([^[\]]+)\]\]?$/.exec(line);
    if (header) {
      current = header[1].trim();
      if (!tables.has(current)) tables.set(current, new Map());
      continue;
    }
    const eq = line.indexOf("=");
    if (eq <= 0) throw new Error(`${label}:${index + 1}: cannot read "${line}"`);
    const key = line.slice(0, eq).trim();
    const value = line.slice(eq + 1).trim();
    depth = balance(value);
    if (depth > 0) {
      pendingKey = key;
      pending = value;
      continue;
    }
    tables.get(current)?.set(key, normalizeTomlValue(value));
  }
  if (depth > 0) throw new Error(`${label}: a value for ${pendingKey} never closes`);
  return tables;
}

/**
 * A normalized TOML array of basic strings, trailing comma allowed; null for
 * anything else, so an array this cannot read is refused rather than read as
 * empty.
 * @param {string} value
 * @returns {string[] | null}
 */
export function parseStringArray(value) {
  if (!value.startsWith("[") || !value.endsWith("]")) return null;
  const body = value.slice(1, -1);
  /** @type {string[]} */
  const items = [];
  const item = /^"((?:[^"\\]|\\.)*)"(,|$)/;
  let rest = body;
  while (rest.length > 0) {
    const match = item.exec(rest);
    if (!match) return null;
    items.push(match[1]);
    rest = rest.slice(match[0].length);
  }
  return items;
}

/**
 * Net opening brackets and braces outside strings.
 * @param {string} text
 */
function balance(text) {
  let depth = 0;
  let quote = "";
  for (let i = 0; i < text.length; i += 1) {
    const ch = text[i];
    if (quote) {
      if (ch === "\\" && quote === '"') i += 1;
      else if (ch === quote) quote = "";
    } else if (ch === '"' || ch === "'") quote = ch;
    else if (ch === "[" || ch === "{") depth += 1;
    else if (ch === "]" || ch === "}") depth -= 1;
  }
  return depth;
}

/**
 * What one member manifest takes from its workspace instead of declaring.
 * @param {Map<string, Map<string, string>>} manifest
 */
export function inheritedKeys(manifest) {
  const fields = [];
  const dependencies = [];
  let lints = false;
  for (const [table, entries] of manifest) {
    for (const [key, value] of entries) {
      const dotted = /^([A-Za-z0-9_-]+)\.workspace$/.exec(key);
      const inherits = (dotted && value === "true") || /(^|[{,])workspace=true([,}]|$)/.test(value);
      if (table === "lints" && key === "workspace" && value === "true") lints = true;
      if (!inherits) continue;
      const name = dotted ? dotted[1] : key;
      if (table === "package") fields.push(name);
      else if (/(^|\.)(dev-|build-)?dependencies$/.test(table)) dependencies.push(name);
    }
  }
  return { fields, dependencies, lints };
}

/**
 * Manvi's `crates/` workspace builds DevCouncil's dc-* crates in place: each
 * member is a symlink into DevCouncil `rust/`, so the sources have one owner.
 * What a member inherits — its version, licence, lints and pinned
 * dependencies — it inherits from Manvi's `crates/Cargo.toml`, though, not
 * from DevCouncil's, and those values were copied by hand. They drifted: the
 * crates built here reported version 0.1.0 under MIT while DevCouncil ships
 * 1.3.5 under Apache-2.0. This compares every value a member inherits with
 * DevCouncil's own, and refuses a member that is a copy rather than a link.
 *
 * DevCouncil's side is read at the commit scripts/module-pins.txt pins, not
 * from whatever the sibling checkout has open: the release builds the pin,
 * so a comparison against a newer checkout would pass on one machine and
 * fail in CI, or the reverse.
 * @param {string} root
 * @param {(relativePath: string) => string} [readPinned] DevCouncil file at
 *   the pinned commit; the default asks git in the sibling checkout.
 * @returns {string[]}
 */
export function crateMirrorProblems(root, readPinned) {
  /** @type {string[]} */
  const errors = [];
  const workspaceFile = path.join(root, "crates", "Cargo.toml");
  let local;
  try {
    local = parseManifest(readFileSync(workspaceFile, "utf8"), "crates/Cargo.toml");
  } catch (err) {
    return [`crates/Cargo.toml: ${err instanceof Error ? err.message : String(err)}`];
  }
  const membersValue = local.get("workspace")?.get("members");
  const members = membersValue === undefined ? null : parseStringArray(membersValue);
  if (members === null) return [`crates/Cargo.toml: cannot read workspace members ${membersValue ?? "(absent)"}`];
  if (members.length === 0) return ["crates/Cargo.toml names no workspace members"];
  let read = readPinned;
  if (!read) {
    let pin;
    try {
      pin = parsePins(readFileSync(path.join(root, "scripts", "module-pins.txt"), "utf8")).find(
        (row) => row.name === "DevCouncil",
      );
    } catch (err) {
      return [`scripts/module-pins.txt: ${err instanceof Error ? err.message : String(err)}`];
    }
    if (!pin) return ["scripts/module-pins.txt does not pin DevCouncil, so the crate mirror has nothing to compare"];
    const checkout = path.join(root, "..", "DevCouncil");
    read = (relative) =>
      execFileSync("git", ["-C", checkout, "show", `${pin.sha}:${relative}`], {
        encoding: "utf8",
        stdio: ["ignore", "pipe", "pipe"],
      });
  }
  /** @type {Map<string, Map<string, string>>} */
  let upstream;
  try {
    upstream = parseManifest(read("rust/Cargo.toml"), "DevCouncil rust/Cargo.toml at the pin");
  } catch (err) {
    return [
      `cannot read DevCouncil rust/Cargo.toml at the pinned commit (${err instanceof Error ? err.message.split("\n")[0] : String(err)}); run scripts/fetch-modules.sh`,
    ];
  }
  /** @type {string | null} */
  let upstreamRoot = null;
  for (const member of members) {
    const link = path.join(root, "crates", member);
    let target;
    try {
      if (!lstatSync(link).isSymbolicLink()) {
        errors.push(`crates/${member} is a copy, not a symlink into DevCouncil rust/: two owners of one crate drift`);
        continue;
      }
      target = realpathSync(link);
    } catch {
      errors.push(`crates/${member} does not resolve; check DevCouncil out beside Manvi (scripts/fetch-modules.sh)`);
      continue;
    }
    const memberRoot = path.dirname(target);
    if (upstreamRoot === null) upstreamRoot = memberRoot;
    else if (memberRoot !== upstreamRoot) {
      errors.push(`crates/${member} links into ${memberRoot}, not ${upstreamRoot}`);
      continue;
    }
    const crate = path.basename(target);
    let manifest;
    try {
      manifest = parseManifest(read(`rust/${crate}/Cargo.toml`), `DevCouncil rust/${crate}/Cargo.toml at the pin`);
    } catch (err) {
      errors.push(`crates/${member}: DevCouncil has no rust/${crate}/Cargo.toml at the pinned commit (${err instanceof Error ? err.message.split("\n")[0] : String(err)})`);
      continue;
    }
    const wanted = inheritedKeys(manifest);
    const compare = (table, key, what) => {
      const theirs = upstream.get(table)?.get(key);
      const ours = local.get(table)?.get(key);
      if (theirs === undefined) {
        errors.push(`${member} inherits ${what} ${key}, which DevCouncil's workspace does not declare`);
      } else if (ours === undefined) {
        errors.push(`${member} inherits ${what} ${key}; crates/Cargo.toml must declare it as DevCouncil does: ${theirs}`);
      } else if (ours !== theirs) {
        errors.push(`${member} inherits ${what} ${key} = ${ours} here but ${theirs} in DevCouncil`);
      }
    };
    for (const field of wanted.fields) compare("workspace.package", field, "package field");
    for (const dependency of wanted.dependencies) compare("workspace.dependencies", dependency, "dependency");
    if (wanted.lints) {
      for (const [table, entries] of upstream) {
        if (!table.startsWith("workspace.lints")) continue;
        for (const key of entries.keys()) compare(table, key, `${table.slice("workspace.".length)} lint`);
      }
    }
  }
  return [...new Set(errors)];
}

/**
 * @param {string} root
 * @param {string} tag
 * @returns {{ tagSha: string | null, headSha: string }}
 */
export function gitTagAndHead(root, tag) {
  const headSha = execFileSync("git", ["-C", root, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  let tagSha = null;
  try {
    tagSha = execFileSync("git", ["-C", root, "rev-parse", `${tag}^{}`], { encoding: "utf8" }).trim();
  } catch {
    tagSha = null;
  }
  return { tagSha, headSha };
}

function parseArgs(argv) {
  const out = {
    tag: "",
    assets: "",
    requireHead: false,
    requirePins: false,
    root: REPO_ROOT,
    help: false,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === "--help" || arg === "-h") out.help = true;
    else if (arg === "--tag") out.tag = argv[++i] || "";
    else if (arg === "--assets") out.assets = argv[++i] || "";
    else if (arg === "--require-head") out.requireHead = true;
    else if (arg === "--require-pins") out.requirePins = true;
    else if (arg === "--root") out.root = argv[++i] || out.root;
    else throw new Error(`unknown argument: ${arg}`);
  }
  return out;
}

/**
 * @param {string[]} argv
 * @returns {number}
 */
export function main(argv = process.argv.slice(2)) {
  let args;
  try {
    args = parseArgs(argv);
  } catch (err) {
    console.error(`check-release: ${err instanceof Error ? err.message : String(err)}`);
    return 2;
  }
  if (args.help) {
    console.log("usage: check-release.mjs [--tag vX.Y.Z] [--require-head] [--require-pins] [--assets dir]");
    return 0;
  }
  /** @type {string[]} */
  const errors = [];
  const root = args.root;
  try {
    errors.push(
      ...workflowProblems({
        releaseYaml: readFileSync(path.join(root, ".github", "workflows", "release.yml"), "utf8"),
        verifyYaml: readFileSync(path.join(root, ".github", "workflows", "verify.yml"), "utf8"),
        publishScript: readFileSync(path.join(root, "scripts", "release-publish.sh"), "utf8"),
        buildScript: readFileSync(path.join(root, "scripts", "release-build.sh"), "utf8"),
      }),
    );
  } catch (err) {
    errors.push(err instanceof Error ? err.message : String(err));
  }
  errors.push(...notesFileProblems(root));
  errors.push(...pinProblems(root, args.requirePins));
  errors.push(...crateMirrorProblems(root));

  if (args.tag) {
    const parsed = parseTag(args.tag);
    if (!parsed.ok) {
      errors.push(parsed.reason);
    } else {
      const notesPath = path.join(root, "docs", "releases", `${parsed.tag}.md`);
      if (!existsSync(notesPath)) errors.push(`missing release notes: ${notesPath}`);
      if (args.requireHead) {
        const found = gitTagAndHead(root, parsed.tag);
        const named = tagNamesHead({ tag: parsed.tag, ...found });
        if (!named.ok) errors.push(named.reason);
      }
      if (args.assets) {
        const assetErrors = assetProblems(args.assets, parsed.tag);
        errors.push(...assetErrors);
        if (assetErrors.length === 0) writeChecksums(args.assets, parsed.tag);
      }
    }
  } else if (args.assets || args.requireHead) {
    errors.push("--assets and --require-head need --tag");
  }

  if (errors.length > 0) {
    for (const error of errors) console.error(`check-release: ${error}`);
    return 1;
  }
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exit(main());
}
