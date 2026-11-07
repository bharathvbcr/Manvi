#!/usr/bin/env bash
# Sign and notarize a macOS release binary, or refuse loudly.
#
#   release-sign-macos.sh preflight      decide, before anything is built
#   release-sign-macos.sh sign <file>    codesign + notarize <file> in place
#
# Secrets (repository or environment secrets named in release.yml; the owner
# adds them, see docs/RELEASING.md):
#   APPLE_CERTIFICATE_P12_BASE64  Developer ID Application certificate and key, .p12, base64
#   APPLE_CERTIFICATE_PASSWORD    the .p12 export password
#   APPLE_SIGNING_IDENTITY        e.g. "Developer ID Application: Name (TEAMID)"
#   APPLE_ID                      Apple ID used for notarytool
#   APPLE_TEAM_ID                 10-character team id
#   APPLE_APP_PASSWORD            app-specific password for APPLE_ID
#
# Missing secrets never produce a quietly unsigned release. The default is
# failure. Only ALLOW_UNSIGNED=true, which release.yml fills from the
# workflow_dispatch input allow_unsigned (a deliberate, per-run choice), lets
# the run continue; the darwin file is then renamed with an -UNSIGNED suffix so
# the artifact name says so.
#
# A bare Mach-O binary cannot carry a stapled notarization ticket. The signed
# binary is what ships; Gatekeeper finds the ticket online. docs/RELEASING.md
# says what that means for offline first launch.
set -euo pipefail

secrets=(
  APPLE_CERTIFICATE_P12_BASE64
  APPLE_CERTIFICATE_PASSWORD
  APPLE_SIGNING_IDENTITY
  APPLE_ID
  APPLE_TEAM_ID
  APPLE_APP_PASSWORD
)

missing=()
for name in "${secrets[@]}"; do
  if [[ -z "${!name:-}" ]]; then missing+=("$name"); fi
done
allow_unsigned=0
case "${ALLOW_UNSIGNED:-}" in true|1) allow_unsigned=1 ;; esac

# Prints "signed" or "unsigned", or exits 1 when signing is impossible and
# not waived. Names only; no value is ever printed.
decide() {
  if [[ "${#missing[@]}" -eq 0 ]]; then
    echo signed
    return 0
  fi
  if [[ "$allow_unsigned" -eq 1 ]]; then
    echo "::warning title=UNSIGNED release::macOS signing secrets are missing (${missing[*]}); allow_unsigned is set, so the darwin binaries are built UNSIGNED and named so." >&2
    echo unsigned
    return 0
  fi
  echo "::error title=macOS signing secrets missing::missing: ${missing[*]}. A release is not built unsigned by accident. Add the secrets (docs/RELEASING.md) or, for a deliberate dry run, set the workflow_dispatch input allow_unsigned." >&2
  return 1
}

cmd="${1:-}"
case "$cmd" in
  preflight)
    state="$(decide)"
    echo "macOS signing: $state"
    if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
      printf 'signing=%s\n' "$state" >> "$GITHUB_OUTPUT"
    fi
    ;;

  sign)
    file="${2:?usage: release-sign-macos.sh sign <file>}"
    test -s "$file" || { echo "no binary at $file" >&2; exit 1; }
    state="$(decide)"
    if [[ "$state" == unsigned ]]; then
      renamed="${file}-UNSIGNED"
      mv "$file" "$renamed"
      echo "UNSIGNED: $renamed"
      if [[ -n "${GITHUB_ENV:-}" ]]; then
        printf 'artifact=%s\n' "$(basename "$renamed")" >> "$GITHUB_ENV"
      fi
      exit 0
    fi

    [[ "$(uname -s)" == Darwin ]] || { echo "signing needs macOS (codesign, notarytool)" >&2; exit 1; }
    work="$(mktemp -d)"
    keychain="${RUNNER_TEMP:-$work}/manvi-signing.keychain-db"
    previous_keychains="$(security list-keychains -d user | tr -d '"' | xargs)"
    cleanup() {
      # shellcheck disable=SC2086
      [[ -n "$previous_keychains" ]] && security list-keychains -d user -s $previous_keychains >/dev/null 2>&1 || true
      security delete-keychain "$keychain" >/dev/null 2>&1 || true
      rm -rf "$work"
    }
    trap cleanup EXIT

    # The certificate lives in a throwaway keychain, removed on exit.
    #
    # argv exposure, stated plainly: `security create-keychain -p`,
    # `unlock-keychain -p`, `set-key-partition-list -k` and `import -P` take
    # their passwords only as arguments (the alternative is a GUI or tty
    # prompt, impossible on a runner). GitHub masks secrets in the job log but
    # NOT in `ps` on the runner. What is exposed is short-lived: the keychain
    # password is a random value for a keychain deleted on exit, and the .p12
    # password only opens a file that is removed right after the import, on an
    # ephemeral single-job VM. The Apple ID app password, the one long-lived
    # credential that would be useful elsewhere, never appears in argv (below).
    kc_pass="$(uuidgen)"
    printf '%s' "$APPLE_CERTIFICATE_P12_BASE64" | base64 --decode > "$work/cert.p12"
    security create-keychain -p "$kc_pass" "$keychain"
    security set-keychain-settings -lut 21600 "$keychain"
    security unlock-keychain -p "$kc_pass" "$keychain"
    # -T names the one tool allowed to use the key; -A (any application) is not used.
    security import "$work/cert.p12" -P "$APPLE_CERTIFICATE_PASSWORD" -t cert -f pkcs12 \
      -T /usr/bin/codesign -k "$keychain"
    security set-key-partition-list -S apple-tool:,apple: -k "$kc_pass" "$keychain" >/dev/null
    # shellcheck disable=SC2086
    security list-keychains -d user -s "$keychain" $previous_keychains
    rm -f "$work/cert.p12"

    # Hardened runtime and a secure timestamp: notarization requires both.
    codesign --force --timestamp --options runtime \
      --sign "$APPLE_SIGNING_IDENTITY" --keychain "$keychain" "$file"
    codesign --verify --strict --verbose=2 "$file"

    # The app password goes into the throwaway keychain through
    # `notarytool store-credentials`, which reads it from its secure prompt, not
    # from argv. The prompt reads a terminal, so it runs under `script`, which
    # supplies a pty and forwards our stdin. Every later notarytool call names
    # the profile, never the password. store-credentials validates the
    # credentials with Apple before saving; a failure stops the job.
    profile="manvi-notary"
    printf '%s\n' "$APPLE_APP_PASSWORD" | script -q /dev/null \
      xcrun notarytool store-credentials "$profile" \
        --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --keychain "$keychain" >/dev/null

    # notarytool takes a zip, pkg or dmg, so the zip is only the submission
    # vehicle; the file that ships is the signed binary.
    ditto -c -k --keepParent "$file" "$work/notarize.zip"
    xcrun notarytool submit "$work/notarize.zip" \
      --keychain-profile "$profile" --keychain "$keychain" \
      --wait --output-format json > "$work/notary.json"
    status="$(jq -r '.status // "unknown"' "$work/notary.json")"
    id="$(jq -r '.id // empty' "$work/notary.json")"
    if [[ "$status" != "Accepted" ]]; then
      echo "notarization of $(basename "$file") ended as: $status (submission ${id:-unknown})" >&2
      if [[ -n "$id" ]]; then
        xcrun notarytool log "$id" --keychain-profile "$profile" --keychain "$keychain" >&2 || true
      fi
      exit 1
    fi
    echo "notarized $(basename "$file") (submission $id)"
    # Signing rewrites the file. It must still start; darwin/amd64 runs
    # under Rosetta on the arm64 runner.
    "$file" version >/dev/null
    ;;

  *)
    echo "usage: release-sign-macos.sh preflight | sign <file>" >&2
    exit 2
    ;;
esac
