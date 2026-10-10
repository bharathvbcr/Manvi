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
 * Sibling DevCouncil and gusset checkouts must be exactly at their pins. A
 * mismatch fails by default; --allow-unpinned (or MANVI_ALLOW_UNPINNED_MODULES=1)
 * turns it into a NOT COVERED line for a developer machine, and is ignored in
 * CI, under --require-pins and for a --tag run.
 *
 * Exit codes: 0 holds · 1 a check failed · 2 the check could not run.
 */
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import {
  closeSync,
  existsSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
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
  if (!releaseYaml.includes("scripts/release-verify-gate.sh") || !/needs:\s*\[[^\]]*\bgate\b/.test(releaseYaml)) {
    errors.push("release.yml does not make publish depend on the verify gate (scripts/release-verify-gate.sh)");
  }
  const checkouts = (releaseYaml.match(/uses:\s*actions\/checkout@/g) ?? []).length;
  const keptCredentials = (releaseYaml.match(/persist-credentials:\s*false/g) ?? []).length;
  if (checkouts === 0 || keptCredentials < checkouts) {
    errors.push("release.yml has a checkout that keeps its credentials; set persist-credentials: false on every one");
  }
  if ((releaseYaml.match(/scripts\/fetch-modules\.sh/g) ?? []).length < 2) {
    errors.push("release.yml does not fetch module replacements in both the build and the publish job");
  }
  if (!/RUST_TOOLCHAIN:\s*"?\d+\.\d+/.test(releaseYaml) || /(--default-toolchain|default|install)\s+stable\b/.test(releaseYaml)) {
    errors.push("release.yml does not pin the Rust toolchain to an exact version");
  }
  // A run script is its own line plus the lines indented deeper than it.
  const lines = releaseYaml.split("\n");
  for (let i = 0; i < lines.length; i += 1) {
    const match = /^([ \t]*)(?:- )?run:(.*)$/.exec(lines[i]);
    if (!match) continue;
    const indent = match[1].length;
    let body = match[2];
    for (let j = i + 1; j < lines.length; j += 1) {
      const next = lines[j];
      if (next.trim() !== "" && next.length - next.trimStart().length <= indent) break;
      body += `\n${next}`;
    }
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
 * Where a sibling checkout stands against its pin, in words an operator can
 * act on: its HEAD, the pin, how many commits apart, and whether the working
 * tree carries edits the pin does not. Returns null when it is exactly the pin.
 *
 * @param {string} dest
 * @param {{ name: string, sha: string }} row
 * @returns {string | null}
 */
function siblingDrift(dest, row) {
  const git = (/** @type {string[]} */ ...args) => execFileSync("git", ["-C", dest, ...args], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  const have = git("rev-parse", "HEAD");
  let dirty = false;
  try {
    dirty = git("status", "--porcelain").length > 0;
  } catch {
    dirty = true;
  }
  if (have === row.sha && !dirty) return null;
  let distance = "unknown";
  try {
    const ahead = git("rev-list", "--count", `${row.sha}..${have}`);
    const behind = git("rev-list", "--count", `${have}..${row.sha}`);
    distance = `${ahead} ahead, ${behind} behind`;
  } catch {
    distance = "the pin is not in this checkout";
  }
  const state = have === row.sha ? "at the pin" : `at ${have.slice(0, 12)}`;
  return `${row.name} is ${state}, pinned ${row.sha.slice(0, 12)} (${distance}), ${dirty ? "with uncommitted changes" : "clean"}. The release builds the pin.`;
}

/**
 * Every sibling module has to be exactly at scripts/module-pins.txt: a local
 * run against a newer DevCouncil or gusset would report a product the tag
 * does not build. A mismatch is an error. With allowUnpinned it is returned
 * as a notCovered line instead, for a developer who means to test against
 * unpinned siblings; the caller prints it and the run does not count as
 * covering the pins.
 *
 * @param {string} root
 * @param {{ allowUnpinned?: boolean }} [options]
 * @returns {{ errors: string[], notCovered: string[] }}
 */
export function pinProblems(root, { allowUnpinned = false } = {}) {
  /** @type {string[]} */
  const errors = [];
  /** @type {string[]} */
  const notCovered = [];
  const file = path.join(root, "scripts", "module-pins.txt");
  if (!existsSync(file)) return { errors: ["missing scripts/module-pins.txt"], notCovered };
  let rows;
  try {
    rows = parsePins(readFileSync(file, "utf8"));
  } catch (err) {
    return { errors: [err instanceof Error ? err.message : String(err)], notCovered };
  }
  const names = new Set(rows.map((row) => row.name));
  for (const required of ["DevCouncil", "gusset"]) {
    if (!names.has(required)) errors.push(`module pins do not name ${required}`);
  }
  for (const row of rows) {
    const dest = path.join(root, "..", row.name);
    /** @type {string | null} */
    let problem;
    if (!existsSync(path.join(dest, ".git"))) {
      problem = `module ${row.name} is not checked out at ${dest}; run scripts/fetch-modules.sh (or scripts/fetch-modules.sh --link-main in a nested worktree)`;
    } else {
      try {
        problem = siblingDrift(dest, row);
      } catch (err) {
        problem = `module ${row.name} at ${dest} could not be read: ${err instanceof Error ? err.message.split("\n")[0] : String(err)}`;
      }
    }
    if (problem) (allowUnpinned ? notCovered : errors).push(problem);
  }
  return { errors, notCovered };
}

/**
 * @typedef {{ mode: string, type: string, name: string }} TreeEntry
 * @typedef {{
 *   read: (relativePath: string) => Buffer | string,
 *   list: (relativeDir: string) => TreeEntry[],
 * }} PinnedTree
 */

/**
 * DevCouncil as scripts/module-pins.txt pins it, read through git in the
 * sibling checkout rather than from its working tree: the release builds the
 * pin, so a check against whatever the checkout has open would pass on one
 * machine and fail in CI, or the reverse. Nothing runs git until `read` or
 * `list` is called, and each throws when the pin cannot be read.
 * @param {string} root
 * @returns {PinnedTree | { problem: string }}
 */
export function pinnedDevCouncil(root) {
  let pin;
  try {
    pin = parsePins(readFileSync(path.join(root, "scripts", "module-pins.txt"), "utf8")).find(
      (row) => row.name === "DevCouncil",
    );
  } catch (err) {
    return { problem: `scripts/module-pins.txt: ${err instanceof Error ? err.message : String(err)}` };
  }
  if (!pin) return { problem: "scripts/module-pins.txt does not pin DevCouncil, so there is nothing to compare against" };
  const { sha } = pin;
  const checkout = path.join(root, "..", "DevCouncil");
  /** @param {string[]} args */
  const git = (args) => execFileSync("git", ["-C", checkout, ...args], { stdio: ["ignore", "pipe", "pipe"] });
  return {
    read: (relative) => git(["show", `${sha}:${relative}`]),
    list: (relativeDir) => {
      // `<sha>:<dir>` names the tree itself: entries come back as basenames,
      // and a directory absent at the pin is a git error, not empty output.
      const out = git(["ls-tree", "-z", `${sha}:${relativeDir}`]).toString("utf8");
      return out
        .split("\0")
        .filter((record) => record !== "")
        .map((record) => {
          const tab = record.indexOf("\t");
          const [mode, type] = record.slice(0, tab).split(" ");
          if (tab < 0 || !mode || !type) throw new Error(`git ls-tree printed an entry it cannot read: ${JSON.stringify(record)}`);
          return { mode, type, name: record.slice(tab + 1) };
        });
    },
  };
}

const firstLine = (/** @type {unknown} */ err) => (err instanceof Error ? err.message.split("\n")[0] : String(err));

/**
 * Contracts Manvi itself owns and DevCouncil never carries: the serve seam is
 * Manvi's wire protocol. They sit beside the vendored files, are recorded in
 * CHECKSUMS like any other, and are held to the Go types by a test in
 * manvi/serve rather than to an upstream copy.
 */
const MANVI_OWNED_CONTRACTS = new Set(["serve.ops.schema.json"]);

/**
 * Manvi vendors DevCouncil's `backend/contracts/` as `contracts/`. The
 * vendored copy must be DevCouncil's at the pin byte for byte, and its
 * CHECKSUMS must describe its own files, or a contract edited here instead
 * of upstream ships unnoticed.
 *
 * The contract set follows DevCouncil's producer-side rule
 * (backend/go_orchestrator/policy/contract_checksums_test.go): every
 * regular, non-hidden file in the directory. Subdirectories such as tools/
 * are machinery, not contract. CHECKSUMS is not compared with upstream
 * byte for byte; it is checked against this copy's own files instead.
 *
 * When the pinned DevCouncil cannot be listed this returns a problem saying
 * the comparison could not run, never an empty list.
 * @param {string} root
 * @param {PinnedTree} [pinnedTree] DevCouncil at the pinned commit; the
 *   default asks git in the sibling checkout.
 * @returns {string[]}
 */
export function contractMirrorProblems(root, pinnedTree) {
  const UPSTREAM_DIR = "backend/contracts";
  /** @type {PinnedTree} */
  let pinned;
  if (pinnedTree) pinned = pinnedTree;
  else {
    const found = pinnedDevCouncil(root);
    if ("problem" in found) return [`contract mirror could not run: ${found.problem}`];
    pinned = found;
  }
  let upstreamNames;
  try {
    upstreamNames = pinned
      .list(UPSTREAM_DIR)
      .filter((entry) => entry.type === "blob" && (entry.mode === "100644" || entry.mode === "100755"))
      .map((entry) => entry.name)
      .filter((name) => !name.startsWith(".") && name !== "CHECKSUMS");
  } catch (err) {
    return [
      `contract mirror could not run: cannot list DevCouncil ${UPSTREAM_DIR} at the pinned commit (${firstLine(err)}); run scripts/fetch-modules.sh`,
    ];
  }
  if (upstreamNames.length === 0) {
    return [`contract mirror could not run: DevCouncil ${UPSTREAM_DIR} has no contract files at the pinned commit`];
  }

  const dir = path.join(root, "contracts");
  let localNames;
  try {
    localNames = readdirSync(dir, { withFileTypes: true })
      .filter((entry) => entry.isFile() && !entry.name.startsWith(".") && entry.name !== "CHECKSUMS")
      .map((entry) => entry.name);
  } catch (err) {
    return [`contract mirror could not run: cannot read contracts/ (${firstLine(err)})`];
  }

  /** @type {string[]} */
  const errors = [];
  const local = new Set(localNames);
  for (const name of [...upstreamNames].sort()) {
    let theirs;
    try {
      theirs = Buffer.from(pinned.read(`${UPSTREAM_DIR}/${name}`));
    } catch (err) {
      errors.push(`contracts/${name}: cannot read DevCouncil ${UPSTREAM_DIR}/${name} at the pinned commit (${firstLine(err)})`);
      continue;
    }
    if (!local.has(name)) {
      errors.push(`contracts/${name} is missing; DevCouncil has ${UPSTREAM_DIR}/${name} at the pinned commit`);
      continue;
    }
    if (!theirs.equals(readFileSync(path.join(dir, name)))) {
      errors.push(`contracts/${name} differs from DevCouncil ${UPSTREAM_DIR}/${name} at the pinned commit`);
    }
  }
  const upstream = new Set(upstreamNames);
  for (const name of [...localNames].sort()) {
    if (!upstream.has(name) && !MANVI_OWNED_CONTRACTS.has(name)) errors.push(`contracts/${name} is not in DevCouncil ${UPSTREAM_DIR} at the pinned commit`);
  }
  errors.push(...checksumsProblems(dir, localNames));
  return errors;
}

/**
 * Whether contracts/CHECKSUMS records exactly `names` at their current
 * digests, in the "<sha256>  <name>" form DevCouncil writes. A line it cannot
 * read is refused rather than skipped.
 * @param {string} dir
 * @param {string[]} names
 * @returns {string[]}
 */
function checksumsProblems(dir, names) {
  let body;
  try {
    body = readFileSync(path.join(dir, "CHECKSUMS"), "utf8");
  } catch (err) {
    return [`contracts/CHECKSUMS: cannot read (${firstLine(err)})`];
  }
  /** @type {Map<string, string>} */
  const recorded = new Map();
  const lines = body.replace(/\n+$/, "").split("\n");
  for (const [i, line] of lines.entries()) {
    const at = line.indexOf("  ");
    const digest = at < 0 ? "" : line.slice(0, at);
    const name = at < 0 ? "" : line.slice(at + 2);
    if (!/^[0-9a-f]{64}$/.test(digest) || name === "") {
      return [`contracts/CHECKSUMS line ${i + 1} is not "<sha256>  <name>": ${JSON.stringify(line)}`];
    }
    if (recorded.has(name)) return [`contracts/CHECKSUMS names ${name} twice`];
    recorded.set(name, digest);
  }
  /** @type {string[]} */
  const drift = [];
  for (const name of names) {
    const actual = createHash("sha256").update(readFileSync(path.join(dir, name))).digest("hex");
    const got = recorded.get(name);
    if (got === undefined) drift.push(`contracts/CHECKSUMS: ${name}: not recorded (actual ${actual})`);
    else if (got !== actual) drift.push(`contracts/CHECKSUMS: ${name}: recorded ${got}, actual ${actual}`);
  }
  for (const name of recorded.keys()) {
    if (!names.includes(name)) drift.push(`contracts/CHECKSUMS: ${name}: recorded but no such contract file`);
  }
  return drift.sort();
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
    allowUnpinned: false,
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
    else if (arg === "--allow-unpinned") out.allowUnpinned = true;
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
    console.log("usage: check-release.mjs [--tag vX.Y.Z] [--require-head] [--require-pins] [--allow-unpinned] [--assets dir]");
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
  // The opt-out is for a developer's machine only: CI, an explicit
  // --require-pins, or a tag run (--tag) never honours it.
  const allowUnpinned =
    (args.allowUnpinned || process.env.MANVI_ALLOW_UNPINNED_MODULES === "1") &&
    !args.requirePins &&
    !args.tag &&
    !process.env.GITHUB_ACTIONS;
  const pins = pinProblems(root, { allowUnpinned });
  errors.push(...pins.errors);
  for (const line of pins.notCovered) console.error(`check-release: NOT COVERED: ${line}`);
  errors.push(...contractMirrorProblems(root));

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
