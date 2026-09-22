import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { after, describe, it } from "node:test";
import {
  NOTES_LIMIT,
  assetProblems,
  expectedBinaries,
  notesProblems,
  parsePins,
  parseTag,
  tagNamesHead,
  workflowProblems,
  writeChecksums,
} from "./check-release.mjs";

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
/** @type {string[]} */
const tempDirs = [];

after(() => {
  for (const dir of tempDirs) rmSync(dir, { recursive: true, force: true });
});

const OLD_RELEASE_YAML = `
concurrency:
  cancel-in-progress: true
    run: |
      if [ "\${{ github.event_name }}" = "workflow_dispatch" ]; then
        VERSION="\${{ inputs.tag }}"
      fi
      generate_release_notes: true
`;

describe("parseTag", () => {
  it("accepts a plain release tag", () => {
    assert.deepEqual(parseTag("v0.0.5"), { ok: true, tag: "v0.0.5", version: "0.0.5" });
  });

  it("rejects a suffix, a missing v, and an empty string", () => {
    for (const tag of ["v0.0.5-rc.1", "0.0.5", "", "v0.0", "vv0.0.5", "v0.0.5 "]) {
      const parsed = parseTag(tag);
      assert.equal(parsed.ok, false, tag);
    }
  });
});

describe("tagNamesHead", () => {
  it("fails when the tag names an older commit", () => {
    const result = tagNamesHead({
      tag: "v0.0.5",
      tagSha: "aaaaaaaaaaaa111111111111",
      headSha: "bbbbbbbbbbbb222222222222",
    });
    assert.equal(result.ok, false);
    if (!result.ok) assert.match(result.reason, /aaaaaaaaaaaa/);
  });

  it("fails when the tag does not exist", () => {
    const result = tagNamesHead({ tag: "v0.0.5", tagSha: null, headSha: "abc" });
    assert.equal(result.ok, false);
  });

  it("passes when the tag is HEAD", () => {
    assert.deepEqual(
      tagNamesHead({ tag: "v0.0.5", tagSha: "abc", headSha: "abc" }),
      { ok: true },
    );
  });
});

describe("notes", () => {
  it("rejects empty notes, a missing version, and a body over the cap", () => {
    assert.match(notesProblems("   ", "v0.0.5", "notes")[0], /empty/);
    assert.match(notesProblems("# hello", "v0.0.5", "notes")[0], /do not mention/);
    const huge = `v0.0.5 ${"x".repeat(NOTES_LIMIT)}`;
    assert.match(notesProblems(huge, "v0.0.5", "notes")[0], /100000/);
  });

  it("accepts the curated v0.0.5 notes", () => {
    const notes = readFileSync(path.join(REPO_ROOT, "docs", "releases", "v0.0.5.md"), "utf8");
    assert.deepEqual(notesProblems(notes, "v0.0.5", "v0.0.5.md"), []);
    assert.ok(notes.length < NOTES_LIMIT);
  });
});

describe("assets", () => {
  it("rejects a missing binary, an empty file, a zip, and an extra name", () => {
    const dir = mkdtempSync(path.join(tmpdir(), "manvi-rel-"));
    tempDirs.push(dir);
    const names = expectedBinaries("v0.0.5");
    assert.deepEqual(assetProblems(dir, "v0.0.5"), names.map((name) => `missing binary: ${name}`));

    for (const name of names) writeFileSync(path.join(dir, name), "binary");
    writeFileSync(path.join(dir, names[0]), Buffer.from([0x50, 0x4b, 0x03, 0x04, 0x00]));
    writeFileSync(path.join(dir, names[1]), Buffer.alloc(0));
    writeFileSync(path.join(dir, "manvi-v0.0.5-windows-amd64"), "nope");
    const problems = assetProblems(dir, "v0.0.5");
    assert.ok(problems.some((line) => line.includes("zip")));
    assert.ok(problems.some((line) => line.includes("empty")));
    assert.ok(problems.some((line) => line.includes("unexpected")));
  });

  it("writes shasum-compatible checksums for a complete set", () => {
    const dir = mkdtempSync(path.join(tmpdir(), "manvi-rel-ok-"));
    tempDirs.push(dir);
    for (const name of expectedBinaries("v0.0.5")) writeFileSync(path.join(dir, name), name);
    assert.deepEqual(assetProblems(dir, "v0.0.5"), []);
    const body = writeChecksums(dir, "v0.0.5");
    assert.match(body, /^[0-9a-f]{64} {2}manvi-v0\.0\.5-darwin-amd64\n/);
    assert.equal(body.trim().split("\n").length, 4);
  });
});

describe("workflow", () => {
  it("rejects the pre-fix release workflow", () => {
    const problems = workflowProblems({
      releaseYaml: OLD_RELEASE_YAML,
      verifyYaml: "run: ./verify.sh\n",
      publishScript: "echo notes",
      buildScript: "go build",
    });
    assert.ok(problems.some((line) => line.includes("--notes-file")));
    assert.ok(problems.some((line) => line.includes("generates notes")));
    assert.ok(problems.some((line) => line.includes("interpolates")));
    assert.ok(problems.some((line) => line.includes("cancels")));
    assert.ok(problems.some((line) => line.includes("module replacements")));
    assert.ok(problems.some((line) => line.includes("verify.sh")));
  });

  it("accepts the workflows in this tree", () => {
    const problems = workflowProblems({
      releaseYaml: readFileSync(path.join(REPO_ROOT, ".github", "workflows", "release.yml"), "utf8"),
      verifyYaml: readFileSync(path.join(REPO_ROOT, ".github", "workflows", "verify.yml"), "utf8"),
      publishScript: readFileSync(path.join(REPO_ROOT, "scripts", "release-publish.sh"), "utf8"),
      buildScript: readFileSync(path.join(REPO_ROOT, "scripts", "release-build.sh"), "utf8"),
    });
    assert.deepEqual(problems, []);
  });
});

describe("pins", () => {
  it("rejects a short sha", () => {
    assert.throws(
      () => parsePins("DevCouncil\thttps://github.com/bharathvbcr/DevCouncil.git\tabc\n"),
      /sha/,
    );
  });

  it("reads the committed pins", () => {
    const rows = parsePins(readFileSync(path.join(REPO_ROOT, "scripts", "module-pins.txt"), "utf8"));
    assert.deepEqual(rows.map((row) => row.name), ["DevCouncil", "gusset"]);
    for (const row of rows) assert.match(row.sha, /^[0-9a-f]{40}$/);
  });
});
