//! Adversarial tests for the rigor gates.
//!
//! The bar for each is the same: a gate that reports a clean pass when it did
//! not actually check is worse than no gate, because it converts an unexamined
//! change into an approved one.

use dc_verify::rigor::*;
use dc_verify::{ChangeStatus, FileDiff, parse_unified};

fn diff_of(path: &str, lines: &[(u32, &str)]) -> FileDiff {
    FileDiff {
        path: path.to_string(),
        old_path: None,
        status: ChangeStatus::Modified,
        added_lines: lines.iter().map(|(n, s)| (*n, s.to_string())).collect(),
        removed_count: 0,
    }
}

// --- secret scanning ---

#[test]
fn credential_shapes_in_added_lines_are_blocking() {
    let files = vec![diff_of(
        "src/client.go",
        &[
            (
                10,
                "const key = \"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA\"",
            ),
            (11, "token := \"ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\""),
            (12, "awsKey := \"AKIAIOSFODNN7EXAMPLE\""),
            (
                13,
                "googleKey := \"AIzaSyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"",
            ),
        ],
    )];
    let findings = scan_secrets(&files);
    assert_eq!(findings.len(), 4, "findings: {findings:#?}");
    for finding in &findings {
        assert_eq!(finding.severity, Severity::Blocking);
        assert_eq!(finding.gate, "secret_scan");
    }
}

#[test]
fn a_secret_finding_never_quotes_the_secret() {
    // The whole point of the gate is that this credential must not spread. A
    // report that prints it in full copies it into the evidence trail, the
    // terminal, and the session log — the exact places the harness works to
    // keep credentials out of.
    let secret = "sk-ant-api03-SUPERSECRETVALUE0123456789";
    let files = vec![diff_of("src/a.go", &[(1, &format!("key := \"{secret}\""))])];
    let findings = scan_secrets(&files);
    assert_eq!(findings.len(), 1);
    let finding = &findings[0];
    assert!(
        !finding.evidence.contains("SUPERSECRET"),
        "the finding quoted the credential: {}",
        finding.evidence
    );
    assert!(!finding.message.contains("SUPERSECRET"));
    assert!(
        finding.evidence.starts_with("sk-ant-"),
        "the finding must still identify the shape: {}",
        finding.evidence
    );
}

#[test]
fn ordinary_code_is_not_flagged_as_a_secret() {
    // A scanner that cries wolf is one whose findings get waved through, which
    // is strictly worse than not having it.
    let files = vec![diff_of(
        "src/a.go",
        &[
            (1, "hash := sha256.Sum256(data)"),
            (
                2,
                "const fixture = \"aGVsbG8gd29ybGQgdGhpcyBpcyBub3QgYSBzZWNyZXQ=\"",
            ),
            (3, "// see https://console.anthropic.com/settings/keys"),
            (4, "id := uuid.New().String()"),
            (5, "sk := computeSortKey(row)"),
            (6, "var akiaCount int"),
        ],
    )];
    let findings = scan_secrets(&files);
    assert!(findings.is_empty(), "false positives: {findings:#?}");
}

#[test]
fn removed_lines_are_not_scanned() {
    // Deleting a secret is the fix, not the offence.
    let mut file = diff_of("src/a.go", &[]);
    file.removed_count = 1;
    assert!(scan_secrets(&[file]).is_empty());
}

// --- stub detection ---

#[test]
fn an_unimplemented_body_blocks() {
    let files = vec![diff_of(
        "src/a.rs",
        &[(5, "    todo!()"), (9, "    unimplemented!()")],
    )];
    let findings = detect_stubs(&files);
    assert_eq!(findings.len(), 2, "{findings:#?}");
    for finding in &findings {
        assert_eq!(finding.severity, Severity::Blocking);
    }
}

#[test]
fn a_todo_comment_is_advisory_not_blocking() {
    let files = vec![diff_of(
        "src/a.go",
        &[(3, "// TODO: handle the retry case")],
    )];
    let findings = detect_stubs(&files);
    assert_eq!(findings.len(), 1);
    assert_eq!(findings[0].severity, Severity::Advisory);
}

#[test]
fn identifiers_containing_marker_words_are_not_flagged() {
    // `todoItems` and `stubServer` are ordinary names. A gate that fires on
    // them is a gate that gets disabled, and then nothing is checked at all.
    let files = vec![diff_of(
        "src/a.go",
        &[
            (1, "todoItems := loadTodos()"),
            (2, "stubServer := httptest.NewServer(handler)"),
            (3, "func (s *Store) MarkTodoDone(id string) error {"),
        ],
    )];
    let findings = detect_stubs(&files);
    assert!(findings.is_empty(), "false positives: {findings:#?}");
}

// --- coverage intersection ---

#[test]
fn uncovered_added_lines_are_reported_with_their_numbers() {
    let files = vec![diff_of(
        "src/a.go",
        &[(10, "x := 1"), (11, "y := 2"), (12, "z := 3")],
    )];
    let coverage = vec![FileCoverage {
        path: "src/a.go".into(),
        covered_lines: vec![10, 12],
    }];
    let report = intersect_coverage(&files, &coverage);
    assert_eq!(report.gaps.len(), 1);
    assert_eq!(report.gaps[0].uncovered_lines, vec![11]);
    assert_eq!(report.gaps[0].added_lines, 3);
    assert!(!report.is_clean());
}

#[test]
fn a_file_with_no_coverage_data_is_unmeasured_not_covered() {
    // This is the finding that justifies the whole report shape. A file with no
    // coverage data and a file with full coverage both yield zero gaps, and
    // conflating them is how "diff coverage passed" comes to mean "coverage was
    // never measured".
    let files = vec![diff_of("src/new.go", &[(1, "func New() {}")])];
    let report = intersect_coverage(&files, &[]);
    assert!(report.gaps.is_empty());
    assert_eq!(report.unmeasured, vec!["src/new.go".to_string()]);
    assert!(
        !report.is_clean(),
        "an unmeasured file must not summarise as clean coverage"
    );
}

#[test]
fn documentation_and_tests_are_not_coverage_questions() {
    let files = vec![
        diff_of("README.md", &[(1, "# title")]),
        diff_of("src/a_test.go", &[(1, "func TestX(t *testing.T) {}")]),
        diff_of("config.yaml", &[(1, "key: value")]),
    ];
    let report = intersect_coverage(&files, &[]);
    assert!(report.is_clean(), "{report:#?}");
}

#[test]
fn a_deleted_file_is_not_a_coverage_gap() {
    let mut file = diff_of("src/gone.go", &[]);
    file.status = ChangeStatus::Deleted;
    file.removed_count = 40;
    let report = intersect_coverage(&[file], &[]);
    assert!(report.is_clean());
}

// --- the gates against a real diff ---

#[test]
fn the_gates_read_a_parsed_diff_end_to_end() {
    // Written as explicit joined lines: a `\` continuation in a Rust string
    // strips the following line's leading whitespace, which silently deletes
    // the leading space that marks a context line.
    let diff = [
        "diff --git a/src/auth.go b/src/auth.go",
        "index 111..222 100644",
        "--- a/src/auth.go",
        "+++ b/src/auth.go",
        "@@ -1,3 +1,6 @@",
        " package auth",
        "+",
        "+// TODO: rotate this",
        "+const key = \"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA\"",
        " func Check() {}",
        "",
    ]
    .join("\n");
    let files = parse_unified(&diff).expect("the diff must parse");
    assert_eq!(files.len(), 1);

    let secrets = scan_secrets(&files);
    assert_eq!(secrets.len(), 1, "{secrets:#?}");
    assert_eq!(secrets[0].path, "src/auth.go");
    assert!(
        secrets[0].line > 0,
        "a finding without a line number is not actionable"
    );

    let stubs = detect_stubs(&files);
    assert_eq!(stubs.len(), 1);
    assert_eq!(stubs[0].severity, Severity::Advisory);

    let coverage = intersect_coverage(&files, &[]);
    assert_eq!(coverage.unmeasured, vec!["src/auth.go".to_string()]);
}

#[test]
fn a_malformed_diff_is_an_error_not_an_empty_clean_result() {
    // Restated at this layer because it is the invariant the gates inherit: an
    // Err here becomes a reported degradation, whereas an empty Ok would become
    // a clean pass over a diff nobody read.
    let broken = "@@ this is not a hunk header @@\n+something\n";
    assert!(parse_unified(broken).is_err());

    // And an empty input is genuinely an empty diff, which must stay Ok — an
    // error there would make "this task changed nothing" indistinguishable
    // from "this input could not be read", in the other direction.
    assert_eq!(parse_unified("").unwrap().len(), 0);
    assert_eq!(parse_unified("   \n\n").unwrap().len(), 0);
}
