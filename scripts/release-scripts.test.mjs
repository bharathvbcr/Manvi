// Tests for the release signing preflight and the rollback script. Neither
// workflow can run here, so the scripts are exercised directly: signing
// through its refusal paths (the codesign path needs macOS secrets), rollback
// against a fake `gh` that records every call.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { after, describe, it } from "node:test";

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
/** @type {string[]} */
const tempDirs = [];
after(() => {
  for (const dir of tempDirs) rmSync(dir, { recursive: true, force: true });
});
function temp(prefix) {
  const dir = mkdtempSync(path.join(tmpdir(), prefix));
  tempDirs.push(dir);
  return dir;
}

const SECRETS = [
  "APPLE_CERTIFICATE_P12_BASE64",
  "APPLE_CERTIFICATE_PASSWORD",
  "APPLE_SIGNING_IDENTITY",
  "APPLE_ID",
  "APPLE_TEAM_ID",
  "APPLE_APP_PASSWORD",
];

/** A clean environment: nothing from the developer's own APPLE_* leaks in. */
function cleanEnv(extra = {}) {
  const env = { ...process.env };
  for (const name of [...SECRETS, "ALLOW_UNSIGNED", "GITHUB_OUTPUT", "GITHUB_ENV"]) delete env[name];
  return { ...env, ...extra };
}

function sign(args, extra) {
  return spawnSync("bash", [path.join(REPO_ROOT, "scripts", "release-sign-macos.sh"), ...args], {
    encoding: "utf8",
    env: cleanEnv(extra),
  });
}

describe("release-sign-macos.sh", () => {
  it("fails loudly, naming the missing secrets and no values, when none are set", () => {
    const result = sign(["preflight"]);
    assert.equal(result.status, 1);
    for (const name of SECRETS) assert.ok(result.stderr.includes(name), name);
    assert.match(result.stderr, /allow_unsigned/);
  });

  it("fails when only some secrets are set, and does not print the one that is", () => {
    const result = sign(["preflight"], { APPLE_ID: "owner@example.com", APPLE_TEAM_ID: "TEAM123456" });
    assert.equal(result.status, 1);
    assert.ok(!result.stderr.includes("owner@example.com"));
    assert.ok(!result.stderr.includes("APPLE_ID,") && !/missing:[^.]*\bAPPLE_ID\b/.test(result.stderr));
    assert.ok(result.stderr.includes("APPLE_APP_PASSWORD"));
  });

  it("with allow_unsigned continues, warns, and records signing=unsigned", () => {
    const out = path.join(temp("manvi-sign-"), "output");
    const result = sign(["preflight"], { ALLOW_UNSIGNED: "true", GITHUB_OUTPUT: out });
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stderr, /UNSIGNED/);
    assert.equal(readFileSync(out, "utf8"), "signing=unsigned\n");
  });

  it("treats allow_unsigned=false as unset", () => {
    assert.equal(sign(["preflight"], { ALLOW_UNSIGNED: "false" }).status, 1);
  });

  it("records signing=signed when every secret is present", () => {
    const out = path.join(temp("manvi-sign-"), "output");
    const secrets = Object.fromEntries(SECRETS.map((name) => [name, "x"]));
    const result = sign(["preflight"], { ...secrets, GITHUB_OUTPUT: out });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(readFileSync(out, "utf8"), "signing=signed\n");
  });

  it("sign refuses without secrets and leaves the binary alone", () => {
    const dir = temp("manvi-sign-");
    const file = path.join(dir, "manvi-v0.0.1-darwin-arm64");
    writeFileSync(file, "binary");
    const result = sign(["sign", file]);
    assert.equal(result.status, 1);
    assert.ok(existsSync(file));
    assert.ok(!existsSync(`${file}-UNSIGNED`));
  });

  it("sign with allow_unsigned renames the file -UNSIGNED and updates the artifact name", () => {
    const dir = temp("manvi-sign-");
    const file = path.join(dir, "manvi-v0.0.1-darwin-arm64");
    const envFile = path.join(dir, "env");
    writeFileSync(file, "binary");
    const result = sign(["sign", file], { ALLOW_UNSIGNED: "true", GITHUB_ENV: envFile });
    assert.equal(result.status, 0, result.stderr);
    assert.ok(!existsSync(file));
    assert.ok(existsSync(`${file}-UNSIGNED`));
    assert.equal(readFileSync(envFile, "utf8"), "artifact=manvi-v0.0.1-darwin-arm64-UNSIGNED\n");
  });

  it("rejects an unknown subcommand", () => {
    assert.equal(sign(["frobnicate"]).status, 2);
  });
});

/** A fake gh on PATH that logs each call and answers the few reads the script makes. */
function fakeGh() {
  const dir = temp("manvi-rollback-");
  const bin = path.join(dir, "bin");
  mkdirSync(bin);
  const log = path.join(dir, "gh.log");
  writeFileSync(log, "");
  const gh = path.join(bin, "gh");
  writeFileSync(
    gh,
    `#!/usr/bin/env bash
echo "$*" >> "$GH_LOG"
case "$1 $2" in
  "release view")
    if [[ " $* " == *" --json assets "* ]]; then printf 'manvi-a\\nmanvi-b\\n'; exit 0; fi
    if [[ " $* " == *isDraft* ]]; then if [[ -n "\${GH_STATE:-}" ]]; then echo "$GH_STATE"; else echo '{"isDraft":false,"isPrerelease":false}'; fi; exit 0; fi
    [[ "$3" == "\${GH_MISSING:-}" ]] && exit 1
    exit 0 ;;
  "release list") echo "$GH_LIST" ;;
esac
exit 0
`,
  );
  chmodSync(gh, 0o755);
  return { bin, log };
}

const LIST = JSON.stringify([
  { tagName: "v0.2.0", publishedAt: "2026-11-01T00:00:00Z" },
  { tagName: "v0.1.0", publishedAt: "2026-10-01T00:00:00Z" },
  { tagName: "v0.0.9", publishedAt: "2026-09-01T00:00:00Z" },
]);

function rollback(extra) {
  const { bin, log } = fakeGh();
  const result = spawnSync("bash", [path.join(REPO_ROOT, "scripts", "release-rollback.sh")], {
    encoding: "utf8",
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      GH_LOG: log,
      GH_LIST: LIST,
      GH_REPO: "owner/repo",
      ROLLBACK_TAG: "v0.2.0",
      CONFIRM_TAG: "v0.2.0",
      ...extra,
    },
  });
  return { result, calls: readFileSync(log, "utf8").split("\n").filter(Boolean) };
}
const mutating = (calls) => calls.filter((c) => /^(release (edit|delete-asset)|api )/.test(c));

describe("release-rollback.sh", () => {
  it("does nothing unless the confirmation equals the tag", () => {
    for (const confirm of ["", "v0.2.1", "v0.2.0 ", "0.2.0"]) {
      const { result, calls } = rollback({ CONFIRM_TAG: confirm });
      assert.equal(result.status, 1, confirm);
      assert.match(result.stderr, /confirmation/);
      assert.deepEqual(calls, [], confirm);
    }
  });

  it("rejects a malformed tag, a bad mode and a bad boolean before any gh call", () => {
    for (const extra of [
      { ROLLBACK_TAG: "v0.2", CONFIRM_TAG: "v0.2" },
      { ROLLBACK_TAG: "latest", CONFIRM_TAG: "latest" },
      { MODE: "delete" },
      { DELETE_TAG: "yes" },
      { DELETE_ASSETS: "1" },
      { PREVIOUS_TAG: "v0.2.0" },
    ]) {
      const { result, calls } = rollback(extra);
      assert.equal(result.status, 1, JSON.stringify(extra));
      assert.deepEqual(calls, [], JSON.stringify(extra));
    }
  });

  it("by default marks the release a pre-release, restores the previous release, and touches nothing else", () => {
    const { result, calls } = rollback({});
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutating(calls), [
      "release edit v0.2.0 --prerelease --latest=false",
      "release edit v0.1.0 --latest",
    ]);
    assert.match(result.stdout, /Git tag v0\.2\.0 left in place/);
  });

  it("draft mode drafts the release", () => {
    const { result, calls } = rollback({ MODE: "draft" });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(mutating(calls)[0], "release edit v0.2.0 --draft --latest=false");
  });

  it("deletes assets only when asked, marking first", () => {
    const { result, calls } = rollback({ DELETE_ASSETS: "true" });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutating(calls), [
      "release edit v0.2.0 --prerelease --latest=false",
      "release delete-asset v0.2.0 manvi-a --yes",
      "release delete-asset v0.2.0 manvi-b --yes",
      "release edit v0.1.0 --latest",
    ]);
  });

  it("deletes the git tag only through delete_tag, last", () => {
    const { result, calls } = rollback({ DELETE_TAG: "true" });
    assert.equal(result.status, 0, result.stderr);
    const m = mutating(calls);
    assert.equal(m[m.length - 1], "api --method DELETE repos/owner/repo/git/refs/tags/v0.2.0");
    assert.ok(!m.some((c) => c.startsWith("release delete-asset")));
  });

  it("uses PREVIOUS_TAG when given, and refuses one that is itself a pre-release", () => {
    const ok = rollback({ PREVIOUS_TAG: "v0.0.9" });
    assert.equal(ok.result.status, 0, ok.result.stderr);
    assert.ok(mutating(ok.calls).includes("release edit v0.0.9 --latest"));
    const bad = rollback({ PREVIOUS_TAG: "v0.0.9", GH_STATE: '{"isDraft":false,"isPrerelease":true}' });
    assert.equal(bad.result.status, 1);
    assert.deepEqual(mutating(bad.calls), []);
  });

  it("fails before changing anything when the release does not exist", () => {
    const { result, calls } = rollback({ GH_MISSING: "v0.2.0" });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /does not exist/);
    assert.deepEqual(mutating(calls), []);
  });

  it("warns, and still withdraws, when there is no earlier release to restore", () => {
    const { result, calls } = rollback({ GH_LIST: JSON.stringify([{ tagName: "v0.2.0", publishedAt: "2026-11-01T00:00:00Z" }]) });
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stderr, /no earlier published release/);
    assert.deepEqual(mutating(calls), ["release edit v0.2.0 --prerelease --latest=false"]);
  });

  it("dry run prints the changes and makes none", () => {
    const { result, calls } = rollback({ ROLLBACK_DRY_RUN: "1", DELETE_ASSETS: "true", DELETE_TAG: "true" });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(mutating(calls), []);
    assert.match(result.stdout, /dry run, would run: gh release edit v0\.2\.0/);
    assert.match(result.stdout, /git\/refs\/tags\/v0\.2\.0/);
  });
});
