import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { lstatSync, mkdirSync, mkdtempSync, readFileSync, readlinkSync, realpathSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { after, describe, it } from "node:test";
import {
  NOTES_LIMIT,
  assetProblems,
  contractMirrorProblems,
  expectedBinaries,
  notesProblems,
  main,
  parsePins,
  pinProblems,
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
    assert.ok(problems.some((line) => line.includes("verify gate")));
    assert.ok(problems.some((line) => line.includes("persist-credentials")));
    assert.ok(problems.some((line) => line.includes("Rust toolchain")));
    assert.ok(problems.some((line) => line.includes("both the build and the publish job")));
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

describe("sibling pins", () => {
  const git = (cwd, ...args) =>
    execFileSync("git", ["-C", cwd, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", ...args], { encoding: "utf8" }).trim();
  /** A Manvi root with sibling DevCouncil and gusset repositories at their pins. */
  function siblings() {
    const top = mkdtempSync(path.join(tmpdir(), "manvi-pins-"));
    tempDirs.push(top);
    const rows = [];
    for (const name of ["DevCouncil", "gusset"]) {
      const dir = path.join(top, name);
      mkdirSync(dir);
      git(dir, "init", "--quiet");
      writeFileSync(path.join(dir, "f"), "1\n");
      git(dir, "add", "-A");
      git(dir, "commit", "--quiet", "-m", "pin");
      rows.push(`${name}\thttps://github.com/x/${name}.git\t${git(dir, "rev-parse", "HEAD")}`);
    }
    const root = path.join(top, "Manvi");
    mkdirSync(path.join(root, "scripts"), { recursive: true });
    writeFileSync(path.join(root, "scripts", "module-pins.txt"), `${rows.join("\n")}\n`);
    return { top, root };
  }
  const advance = (dir) => {
    writeFileSync(path.join(dir, "f"), "2\n");
    git(dir, "commit", "--quiet", "-am", "later");
  };

  it("is silent when both siblings are exactly at their pins", () => {
    assert.deepEqual(pinProblems(siblings().root), { errors: [], notCovered: [] });
  });

  it("fails by default on a sibling past its pin, naming HEAD, pin and distance", () => {
    const { top, root } = siblings();
    advance(path.join(top, "DevCouncil"));
    const { errors, notCovered } = pinProblems(root);
    assert.equal(errors.length, 1, errors.join("\n"));
    assert.match(errors[0], /^DevCouncil is at [0-9a-f]{12}, pinned [0-9a-f]{12} \(1 ahead, 0 behind\), clean\./);
    assert.deepEqual(notCovered, []);
  });

  it("reports uncommitted edits in a sibling that is at the pin", () => {
    const { top, root } = siblings();
    writeFileSync(path.join(top, "gusset", "f"), "edited\n");
    const { errors } = pinProblems(root);
    assert.match(errors[0], /^gusset is at the pin, pinned [0-9a-f]{12} \(0 ahead, 0 behind\), with uncommitted changes/);
  });

  it("moves a mismatch to NOT COVERED only when the opt-out is given", () => {
    const { top, root } = siblings();
    advance(path.join(top, "gusset"));
    const out = pinProblems(root, { allowUnpinned: true });
    assert.deepEqual(out.errors, []);
    assert.equal(out.notCovered.length, 1);
    assert.match(out.notCovered[0], /^gusset is at /);
  });

  it("names the bootstrap when a sibling is absent", () => {
    const { top, root } = siblings();
    rmSync(path.join(top, "DevCouncil"), { recursive: true });
    assert.match(pinProblems(root).errors[0], /module DevCouncil is not checked out at .*run scripts\/fetch-modules\.sh/);
  });

  it("main fails closed with no sibling checkout, as the publish job would see it, and prints what to run", () => {
    const { top, root } = siblings();
    rmSync(path.join(top, "DevCouncil"), { recursive: true });
    const lines = [];
    const original = console.error;
    console.error = (line) => lines.push(String(line));
    try {
      assert.equal(main(["--root", root]), 1);
    } finally {
      console.error = original;
    }
    assert.ok(lines.some((l) => l.includes("fetch-modules.sh")), lines.join("\n"));
  });

  it("main honours the opt-out locally and never for a tag run", () => {
    const { top, root } = siblings();
    advance(path.join(top, "DevCouncil"));
    const lines = [];
    const original = console.error;
    console.error = (line) => lines.push(String(line));
    const saved = process.env.GITHUB_ACTIONS;
    delete process.env.GITHUB_ACTIONS;
    try {
      main(["--root", root, "--allow-unpinned"]);
      assert.ok(lines.some((l) => l.startsWith("check-release: NOT COVERED: DevCouncil is at")), lines.join("\n"));
      lines.length = 0;
      main(["--root", root, "--allow-unpinned", "--tag", "v1.2.3"]);
      assert.equal(lines.some((l) => l.includes("NOT COVERED")), false, lines.join("\n"));
      assert.ok(lines.some((l) => l.startsWith("check-release: DevCouncil is at")), lines.join("\n"));
    } finally {
      console.error = original;
      if (saved !== undefined) process.env.GITHUB_ACTIONS = saved;
    }
  });
});

describe("fetch-modules --link-main", () => {
  const git = (cwd, ...args) =>
    execFileSync("git", ["-C", cwd, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", ...args], { encoding: "utf8" }).trim();
  /**
   * top/Manvi is the main worktree with the scripts committed and sibling
   * checkouts top/DevCouncil and top/gusset at the pins; top/Manvi/.wt/x is a
   * linked worktree nested inside it, where ../DevCouncil does not exist.
   */
  function nested() {
    const top = realpathSync(mkdtempSync(path.join(tmpdir(), "manvi-link-")));
    tempDirs.push(top);
    const rows = [];
    for (const name of ["DevCouncil", "gusset"]) {
      const dir = path.join(top, name);
      mkdirSync(dir);
      git(dir, "init", "--quiet");
      writeFileSync(path.join(dir, "f"), "1\n");
      git(dir, "add", "-A");
      git(dir, "commit", "--quiet", "-m", "pin");
      rows.push(`${name}\thttps://github.com/x/${name}.git\t${git(dir, "rev-parse", "HEAD")}`);
    }
    const main = path.join(top, "Manvi");
    mkdirSync(path.join(main, "scripts"), { recursive: true });
    writeFileSync(path.join(main, "scripts", "module-pins.txt"), `${rows.join("\n")}\n`);
    writeFileSync(path.join(main, "scripts", "fetch-modules.sh"), readFileSync(path.join(REPO_ROOT, "scripts", "fetch-modules.sh")));
    git(main, "init", "--quiet");
    git(main, "add", "-A");
    git(main, "commit", "--quiet", "-m", "init");
    const wt = path.join(main, ".wt", "x");
    git(main, "worktree", "add", "--quiet", "--detach", wt);
    return { top, main, wt };
  }
  const run = (cwd, ...args) => spawnSync("bash", [path.join(cwd, "scripts", "fetch-modules.sh"), ...args], { encoding: "utf8" });

  it("links both siblings from the main worktree and is idempotent", () => {
    const { top, wt } = nested();
    const first = run(wt, "--link-main");
    assert.equal(first.status, 0, first.stderr);
    for (const name of ["DevCouncil", "gusset"]) {
      assert.equal(lstatSync(path.join(wt, "..", name)).isSymbolicLink(), true);
      assert.equal(readlinkSync(path.join(wt, "..", name)), path.join(top, name));
    }
    const again = run(wt, "--link-main");
    assert.equal(again.status, 0, again.stderr);
    assert.match(again.stdout, /already resolves/);
  });

  it("refuses to replace an existing directory", () => {
    const { wt } = nested();
    mkdirSync(path.join(wt, "..", "DevCouncil"));
    const out = run(wt, "--link-main");
    assert.notEqual(out.status, 0);
    assert.match(out.stderr, /refusing to replace .*DevCouncil/);
  });

  it("refuses a main-worktree sibling that is not at the pin", () => {
    const { top, wt } = nested();
    writeFileSync(path.join(top, "gusset", "f"), "2\n");
    git(path.join(top, "gusset"), "commit", "--quiet", "-am", "later");
    const out = run(wt, "--link-main");
    assert.notEqual(out.status, 0);
    assert.match(out.stderr, /refusing to link .*gusset: it is at/);
  });

  it("refuses --link-main in the main worktree and an unknown argument", () => {
    const { main, wt } = nested();
    const inMain = run(main, "--link-main");
    assert.notEqual(inMain.status, 0);
    assert.match(inMain.stderr, /is the main worktree/);
    assert.equal(run(wt, "--bogus").status, 2);
  });
});

describe("verify gate", () => {
  const SHA = "a".repeat(40);
  /** Runs release-verify-gate.sh with a fake `gh` that prints rows, or fails. */
  function gate(rows, { ghFails = false, commit = SHA } = {}) {
    const dir = mkdtempSync(path.join(tmpdir(), "manvi-gate-"));
    tempDirs.push(dir);
    writeFileSync(path.join(dir, "rows"), rows.map((r) => r.join("\t")).join("\n") + (rows.length ? "\n" : ""));
    writeFileSync(path.join(dir, "gh"), `#!/bin/sh\n${ghFails ? "exit 1" : `cat "${dir}/rows"`}\n`, { mode: 0o755 });
    return spawnSync("bash", [path.join(REPO_ROOT, "scripts", "release-verify-gate.sh")], {
      encoding: "utf8",
      env: { ...process.env, PATH: `${dir}:${process.env.PATH}`, RELEASE_COMMIT: commit, GH_REPO: "o/r" },
    });
  }

  it("passes on a completed successful verify run at the commit and prints its url", () => {
    const out = gate([["11", SHA, "completed", "failure", "u/11"], ["12", SHA, "completed", "success", "u/12"]]);
    assert.equal(out.status, 0, out.stderr);
    assert.equal(out.stdout.trim().split("\n").pop(), "u/12");
  });

  it("refuses a failed, unfinished, other-commit or absent run", () => {
    for (const rows of [
      [["1", SHA, "completed", "failure", "u"]],
      [["1", SHA, "in_progress", "", "u"]],
      [["1", "b".repeat(40), "completed", "success", "u"]],
      [],
    ]) {
      const out = gate(rows);
      assert.equal(out.status, 1, JSON.stringify(rows));
      assert.match(out.stderr, /no successful verify run/);
    }
  });

  it("refuses when gh itself fails, and a commit that is not a full id", () => {
    assert.equal(gate([], { ghFails: true }).status, 1);
    assert.equal(gate([["1", SHA, "completed", "success", "u"]], { commit: "main" }).status, 1);
  });
});

describe("contract mirror", () => {
  const CONTRACTS = {
    "README.md": "# contracts\n",
    "event.schema.json": '{"type":"object"}\n',
    "lease.schema.md": "# lease\n",
  };
  const sha256 = (body) => createHash("sha256").update(body).digest("hex");
  const checksumsFor = (files) =>
    Object.entries(files)
      .map(([name, body]) => `${sha256(body)}  ${name}\n`)
      .join("");
  /**
   * A Manvi root whose contracts/ holds `files`, plus CHECKSUMS (recorded
   * from `recorded`, which defaults to `files`) and the noise a real copy
   * carries: a tools/ subdirectory and a hidden file.
   */
  function manvi(files = CONTRACTS, { recorded = files, checksums } = {}) {
    const root = mkdtempSync(path.join(tmpdir(), "manvi-contracts-"));
    tempDirs.push(root);
    const dir = path.join(root, "contracts");
    mkdirSync(path.join(dir, "tools"), { recursive: true });
    for (const [name, body] of Object.entries(files)) writeFileSync(path.join(dir, name), body);
    writeFileSync(path.join(dir, "CHECKSUMS"), checksums ?? checksumsFor(recorded));
    writeFileSync(path.join(dir, "tools", "checksums.py"), "machinery, not contract\n");
    writeFileSync(path.join(dir, ".DS_Store"), "finder noise\n");
    return root;
  }
  /** DevCouncil backend/contracts at the pin, as git ls-tree and git show see it. */
  function upstream(files = CONTRACTS, { unreadable = [], extraEntries = [] } = {}) {
    /** @type {Record<string, string>} */
    const all = { ...files, CHECKSUMS: checksumsFor(files) };
    return {
      list: (/** @type {string} */ dir) => {
        assert.equal(dir, "backend/contracts");
        return [
          ...Object.keys(all).map((name) => ({ mode: "100644", type: "blob", name })),
          { mode: "040000", type: "tree", name: "tools" },
          ...extraEntries,
        ];
      },
      read: (/** @type {string} */ relative) => {
        const name = relative.replace(/^backend\/contracts\//, "");
        if (unreadable.includes(name) || !(name in all)) {
          throw new Error(`fatal: path '${relative}' does not exist in the pin`);
        }
        return Buffer.from(all[name]);
      },
    };
  }

  it("holds when every contract file matches DevCouncil's at the pin", () => {
    // tools/, .DS_Store and an upstream symlink are not contract files.
    const pinned = upstream(CONTRACTS, { extraEntries: [{ mode: "120000", type: "blob", name: "link.md" }] });
    assert.deepEqual(contractMirrorProblems(manvi(), pinned), []);
  });

  it("names a contract file edited here", () => {
    const edited = { ...CONTRACTS, "event.schema.json": '{"type":"array"}\n' };
    const problems = contractMirrorProblems(manvi(edited), upstream());
    assert.ok(
      problems.includes("contracts/event.schema.json differs from DevCouncil backend/contracts/event.schema.json at the pinned commit"),
      problems.join("\n"),
    );
  });

  it("names a contract file missing here", () => {
    /** @type {Record<string, string>} */
    const files = { ...CONTRACTS };
    delete files["lease.schema.md"];
    const problems = contractMirrorProblems(manvi(files, { recorded: CONTRACTS }), upstream());
    assert.ok(
      problems.includes("contracts/lease.schema.md is missing; DevCouncil has backend/contracts/lease.schema.md at the pinned commit"),
      problems.join("\n"),
    );
  });

  it("names a contract file here that DevCouncil does not have", () => {
    const problems = contractMirrorProblems(manvi({ ...CONTRACTS, "extra.json": "[]\n" }), upstream());
    assert.ok(
      problems.includes("contracts/extra.json is not in DevCouncil backend/contracts at the pinned commit"),
      problems.join("\n"),
    );
  });

  it("names an upstream contract file it cannot read", () => {
    const problems = contractMirrorProblems(manvi(), upstream(CONTRACTS, { unreadable: ["README.md"] }));
    assert.ok(
      problems.some((p) => p.startsWith("contracts/README.md: cannot read DevCouncil backend/contracts/README.md at the pinned commit")),
      problems.join("\n"),
    );
  });

  it("reports that the comparison could not run, rather than returning nothing", () => {
    const unlistable = {
      list: () => {
        throw new Error("fatal: not a git repository: ../DevCouncil\nmore");
      },
      read: () => {
        throw new Error("unreachable");
      },
    };
    const problems = contractMirrorProblems(manvi(), unlistable);
    assert.equal(problems.length, 1, problems.join("\n"));
    assert.match(
      problems[0],
      /^contract mirror could not run: cannot list DevCouncil backend\/contracts at the pinned commit \(fatal: not a git repository/,
    );

    const empty = { list: () => [], read: unlistable.read };
    const none = contractMirrorProblems(manvi(), empty);
    assert.equal(none.length, 1, none.join("\n"));
    assert.match(none[0], /^contract mirror could not run: DevCouncil backend\/contracts has no contract files at the pinned commit/);
  });

  it("reports that the comparison could not run when no DevCouncil checkout is beside Manvi", () => {
    // The default reader, not an injected one: a pin with nothing to read it from.
    const root = manvi();
    mkdirSync(path.join(root, "scripts"));
    writeFileSync(
      path.join(root, "scripts", "module-pins.txt"),
      `DevCouncil\thttps://github.com/bharathvbcr/DevCouncil.git\t${"a".repeat(40)}\n`,
    );
    const problems = contractMirrorProblems(root);
    assert.equal(problems.length, 1, problems.join("\n"));
    assert.match(problems[0], /^contract mirror could not run: /);
  });

  it("lets a contract Manvi owns sit beside the vendored ones, and still records its checksum", () => {
    // The serve seam is Manvi's own wire protocol: DevCouncil never has it.
    const owned = { ...CONTRACTS, "serve.ops.schema.json": "{}\n" };
    assert.deepEqual(contractMirrorProblems(manvi(owned), upstream()), []);

    const unrecorded = contractMirrorProblems(manvi(owned, { recorded: CONTRACTS }), upstream());
    assert.ok(
      unrecorded.some((p) => p.startsWith("contracts/CHECKSUMS: serve.ops.schema.json: not recorded")),
      unrecorded.join("\n"),
    );
  });

  it("checks that this copy's CHECKSUMS describes its own files", () => {
    // Every file matches upstream byte for byte; only the manifest is stale.
    const stale = { ...CONTRACTS, "README.md": "# old readme\n" };
    const problems = contractMirrorProblems(manvi(CONTRACTS, { recorded: stale }), upstream());
    assert.deepEqual(problems, [
      `contracts/CHECKSUMS: README.md: recorded ${sha256("# old readme\n")}, actual ${sha256("# contracts\n")}`,
    ]);

    const added = { ...CONTRACTS, "new.json": "{}\n" };
    const unrecorded = contractMirrorProblems(manvi(added, { recorded: CONTRACTS }), upstream(added));
    assert.ok(unrecorded.some((p) => p.startsWith("contracts/CHECKSUMS: new.json: not recorded")), unrecorded.join("\n"));

    const malformed = contractMirrorProblems(manvi(CONTRACTS, { checksums: "not a checksum line\n" }), upstream());
    assert.ok(malformed.some((p) => p.startsWith("contracts/CHECKSUMS line 1 is not")), malformed.join("\n"));
  });
});
