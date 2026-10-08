import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { after, describe, it } from "node:test";
import {
  NOTES_LIMIT,
  assetProblems,
  crateMirrorProblems,
  inheritedKeys,
  parseManifest,
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

describe("crate mirror", () => {
  const UPSTREAM_WORKSPACE = `[workspace]
members = ["dc-a", "dc-b"]

[workspace.package]
version = "1.3.5"
edition = "2021"
license = "Apache-2.0"

[workspace.lints.rust]
unsafe_code = "forbid"

[workspace.dependencies]
sha2 = "0.10.9"
rusqlite = { version = "0.40.2", features = ["bundled"] }
`;
  const MEMBER_A = `[package]
name = "dc-a"
version.workspace = true
edition = "2021"
license.workspace = true

[dependencies]
sha2.workspace = true

[lints]
workspace = true
`;
  const MEMBER_B = `[package]
name = "dc-b"
version.workspace = true
license.workspace = true

[dependencies]
rusqlite = { workspace = true }
dc-a = { path = "../dc-a" }
`;
  const LOCAL = `[workspace]
resolver = "3"
members = [
  "dc-a", # the first
  "dc-b",
]

[workspace.package]
version = "1.3.5"
edition = "2024"
license = "Apache-2.0"

[workspace.lints.rust]
unsafe_code = "forbid"

[workspace.dependencies]
sha2 = "0.10.9"
rusqlite = {version="0.40.2",features=["bundled"]}
`;
  /** A Manvi root whose crates/* link into a DevCouncil rust/ beside it. */
  function layout(local = LOCAL, { copy = [] } = {}) {
    const top = mkdtempSync(path.join(tmpdir(), "manvi-mirror-"));
    tempDirs.push(top);
    const rust = path.join(top, "DevCouncil", "rust");
    for (const crate of ["dc-a", "dc-b"]) mkdirSync(path.join(rust, crate), { recursive: true });
    const root = path.join(top, "Manvi");
    mkdirSync(path.join(root, "crates"), { recursive: true });
    writeFileSync(path.join(root, "crates", "Cargo.toml"), local);
    for (const crate of ["dc-a", "dc-b"]) {
      if (copy.includes(crate)) mkdirSync(path.join(root, "crates", crate));
      else symlinkSync(path.join("..", "..", "DevCouncil", "rust", crate), path.join(root, "crates", crate));
    }
    return root;
  }
  const pinned = (files) => (relative) => {
    if (!(relative in files)) throw new Error(`fatal: path '${relative}' does not exist in the pin`);
    return files[relative];
  };
  const UPSTREAM = {
    "rust/Cargo.toml": UPSTREAM_WORKSPACE,
    "rust/dc-a/Cargo.toml": MEMBER_A,
    "rust/dc-b/Cargo.toml": MEMBER_B,
  };

  it("holds when every inherited value equals DevCouncil's at the pin", () => {
    // Whitespace inside an inline table and a multi-line members array with a
    // comment are spellings, not differences.
    assert.deepEqual(crateMirrorProblems(layout(), pinned(UPSTREAM)), []);
  });

  it("names a drifted version and licence, per member", () => {
    const local = LOCAL.replace('version = "1.3.5"', 'version = "0.1.0"').replace('license = "Apache-2.0"', 'license = "MIT"');
    const problems = crateMirrorProblems(layout(local), pinned(UPSTREAM));
    assert.ok(problems.includes('dc-a inherits package field version = "0.1.0" here but "1.3.5" in DevCouncil'), problems.join("\n"));
    assert.ok(problems.includes('dc-b inherits package field license = "MIT" here but "Apache-2.0" in DevCouncil'), problems.join("\n"));
  });

  it("ignores fields a member declares itself", () => {
    // Both members set or omit edition themselves; 2024 here differs from
    // 2021 upstream and must not be reported.
    assert.equal(crateMirrorProblems(layout(), pinned(UPSTREAM)).some((p) => p.includes("edition")), false);
  });

  it("refuses an inherited dependency this workspace forgot or pinned differently", () => {
    const missing = LOCAL.replace('sha2 = "0.10.9"\n', "");
    assert.ok(
      crateMirrorProblems(layout(missing), pinned(UPSTREAM)).includes(
        'dc-a inherits dependency sha2; crates/Cargo.toml must declare it as DevCouncil does: "0.10.9"',
      ),
    );
    const drifted = LOCAL.replace('features=["bundled"]', 'features=[]');
    assert.ok(crateMirrorProblems(layout(drifted), pinned(UPSTREAM)).some((p) => p.startsWith("dc-b inherits dependency rusqlite =")));
  });

  it("compares the lints a member inherits", () => {
    const local = LOCAL.replace('unsafe_code = "forbid"', 'unsafe_code = "warn"');
    assert.ok(crateMirrorProblems(layout(local), pinned(UPSTREAM)).some((p) => p.startsWith("dc-a inherits lints.rust lint unsafe_code")));
  });

  it("refuses a member that is a copy instead of a link", () => {
    const problems = crateMirrorProblems(layout(LOCAL, { copy: ["dc-b"] }), pinned(UPSTREAM));
    assert.ok(problems.some((p) => p.startsWith("crates/dc-b is a copy")), problems.join("\n"));
  });

  it("fails, rather than passing, when the pinned DevCouncil cannot be read", () => {
    const problems = crateMirrorProblems(layout(), pinned({}));
    assert.equal(problems.length, 1);
    assert.match(problems[0], /cannot read DevCouncil rust\/Cargo.toml at the pinned commit/);
  });

  it("reports a member DevCouncil does not have at the pin", () => {
    const problems = crateMirrorProblems(layout(), pinned({ ...UPSTREAM, "rust/dc-b/Cargo.toml": undefined }));
    // An own key whose value is undefined still answers `in`; drop it.
    const files = { ...UPSTREAM };
    delete files["rust/dc-b/Cargo.toml"];
    const missing = crateMirrorProblems(layout(), pinned(files));
    assert.ok(missing.some((p) => p.startsWith("crates/dc-b: DevCouncil has no rust/dc-b/Cargo.toml")), missing.join("\n"));
    assert.ok(Array.isArray(problems));
  });

  it("refuses a manifest line it cannot read rather than skipping a key", () => {
    assert.throws(() => parseManifest("[package]\nname\n", "x"), /x:2: cannot read "name"/);
    assert.throws(() => parseManifest('[workspace]\nmembers = [\n  "a",\n', "x"), /never closes/);
  });

  it("finds every way a member inherits", () => {
    assert.deepEqual(inheritedKeys(parseManifest(MEMBER_A, "a")), { fields: ["version", "license"], dependencies: ["sha2"], lints: true });
    assert.deepEqual(inheritedKeys(parseManifest(MEMBER_B, "b")), { fields: ["version", "license"], dependencies: ["rusqlite"], lints: false });
  });

  it("holds for this repository at its pin", () => {
    // The real thing: crates/Cargo.toml against DevCouncil at module-pins.txt.
    // Skipped, and said so, only when no DevCouncil checkout is beside Manvi.
    const checkout = path.join(REPO_ROOT, "..", "DevCouncil", ".git");
    let present = true;
    try {
      readFileSync(path.join(REPO_ROOT, "..", "DevCouncil", "rust", "Cargo.toml"));
    } catch {
      present = false;
    }
    if (!present) {
      console.log(`crate mirror: no DevCouncil checkout at ${checkout}; run scripts/fetch-modules.sh`);
      return;
    }
    assert.deepEqual(crateMirrorProblems(REPO_ROOT), []);
  });
});
