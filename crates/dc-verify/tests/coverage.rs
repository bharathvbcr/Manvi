//! Coverage ingestion tests.
//!
//! The governing rule: a coverage file that could not be read is an error, and
//! never an empty measurement. An empty measurement is the strongest finding
//! the gate can make — "these lines ran nothing" — and manufacturing it from a
//! broken pipeline turns an infrastructure fault into a wall of false gaps
//! against the author's code.

use dc_verify::coverage::parse;
use dc_verify::rigor::{FileCoverage, intersect_coverage};
use dc_verify::{ChangeStatus, FileDiff};

const GO_PROFILE: &str = "mode: set\n\
manvi/gate/gate.go:10.20,14.2 3 1\n\
manvi/gate/gate.go:16.30,18.2 2 0\n\
manvi/policy/file.go:5.10,6.2 1 1\n";

const LCOV: &str = "TN:\n\
SF:crates/dc-verify/src/lib.rs\n\
DA:10,4\n\
DA:11,0\n\
DA:12,1\n\
end_of_record\n";

#[test]
fn a_go_profile_expands_blocks_into_covered_lines() {
    let files = parse(GO_PROFILE).expect("the profile must parse");
    let gate = files
        .iter()
        .find(|f| f.path == "gate/gate.go")
        .unwrap_or_else(|| panic!("module prefix not stripped: {:?}", files));
    // The executed block spans 10..=14; the zero-count block 16..=18 does not.
    assert_eq!(gate.covered_lines, vec![10, 11, 12, 13, 14]);
    assert!(files.iter().any(|f| f.path == "policy/file.go"));
}

#[test]
fn lcov_reads_only_executed_lines() {
    let files = parse(LCOV).expect("LCOV must parse");
    assert_eq!(files.len(), 1);
    assert_eq!(files[0].path, "crates/dc-verify/src/lib.rs");
    assert_eq!(files[0].covered_lines, vec![10, 12]);
}

#[test]
fn an_lcov_checksum_field_does_not_break_the_hit_count() {
    let files = parse("SF:a.rs\nDA:3,7,abc123\nend_of_record\n").expect("must parse");
    assert_eq!(files[0].covered_lines, vec![3]);
}

#[test]
fn a_broken_coverage_file_is_an_error_not_an_empty_measurement() {
    // Each of these would otherwise yield "no lines covered", which reads as
    // the strongest possible finding about the code under test.
    let cases: &[(&str, &str)] = &[
        ("empty", ""),
        ("whitespace only", "   \n\n"),
        ("unknown format", "{\"coverage\": 91.2}\n"),
        ("go header with no blocks", "mode: set\n"),
        ("go block with no count", "mode: set\ngate.go:10.20,14.2\n"),
        (
            "go block with a bad number",
            "mode: set\ngate.go:10.20,14.2 3 many\n",
        ),
        (
            "go block ending before it starts",
            "mode: set\ngate.go:14.2,10.20 3 1\n",
        ),
        ("lcov with no DA records", "TN:\nSF:a.rs\nend_of_record\n"),
        ("lcov DA before SF", "TN:\nDA:3,1\n"),
        ("lcov with an empty SF", "SF:\nDA:3,1\n"),
        ("lcov with a bad hit count", "SF:a.rs\nDA:3,lots\n"),
    ];
    for (name, raw) in cases {
        assert!(
            parse(raw).is_err(),
            "{name}: parsed into a measurement instead of erroring"
        );
    }
}

#[test]
fn a_parse_error_names_the_line_so_it_is_actionable() {
    let err = parse("mode: set\ngate.go:10.20,14.2 3 many\n").unwrap_err();
    assert_eq!(err.line_number, 2);
    assert!(err.line.contains("many"));
    assert!(!err.reason.is_empty());
}

#[test]
fn real_coverage_closes_the_unmeasured_gap() {
    // The end-to-end claim: before coverage was fed in, every changed source
    // file came back unmeasured and `passed` could not mean the change was
    // exercised. With it, an executed line is covered and an unexecuted one is
    // a gap — two different findings where there used to be one.
    let files = vec![FileDiff {
        path: "gate/gate.go".into(),
        old_path: None,
        status: ChangeStatus::Modified,
        added_lines: vec![
            (10, "a := 1".into()),
            (11, "b := 2".into()),
            (17, "unreached()".into()),
        ],
        removed_count: 0,
    }];

    let blind = intersect_coverage(&files, &[]);
    assert_eq!(blind.unmeasured, vec!["gate/gate.go".to_string()]);
    assert!(!blind.is_clean());

    let measured = intersect_coverage(&files, &parse(GO_PROFILE).unwrap());
    assert!(
        measured.unmeasured.is_empty(),
        "the file was measured: {measured:#?}"
    );
    assert_eq!(measured.gaps.len(), 1);
    assert_eq!(measured.gaps[0].uncovered_lines, vec![17]);
    assert!(!measured.is_clean());
}

#[test]
fn a_fully_covered_diff_is_clean() {
    let files = vec![FileDiff {
        path: "gate/gate.go".into(),
        old_path: None,
        status: ChangeStatus::Modified,
        added_lines: vec![(10, "a := 1".into()), (12, "c := 3".into())],
        removed_count: 0,
    }];
    let report = intersect_coverage(&files, &parse(GO_PROFILE).unwrap());
    assert!(report.is_clean(), "{report:#?}");
}

#[test]
fn coverage_for_a_file_the_diff_never_touched_is_ignored() {
    let files = vec![FileDiff {
        path: "gate/gate.go".into(),
        old_path: None,
        status: ChangeStatus::Modified,
        added_lines: vec![(10, "a := 1".into())],
        removed_count: 0,
    }];
    let mut measurements = parse(GO_PROFILE).unwrap();
    measurements.push(FileCoverage {
        path: "somewhere/else.go".into(),
        covered_lines: vec![1, 2, 3],
    });
    assert!(intersect_coverage(&files, &measurements).is_clean());
}

#[test]
fn an_absurd_span_is_malformed_not_materialised() {
    // One profile line used to allocate its full span line by line: a corrupt
    // or hostile profile could push ~4.3 billion u32s before the intersection
    // ever ran.
    let profile = "mode: set\nmod/file.go:1.1,4294967295.1 1 1\n";
    let err = dc_verify::coverage::parse(profile).expect_err("an absurd span must be refused");
    assert!(
        err.reason.contains("more lines than any real source file"),
        "unexpected reason: {}",
        err.reason
    );
}

#[test]
fn absolute_lcov_paths_reduce_against_the_supplied_root() {
    // llvm-cov and grcov emit SF: as an absolute path. Without a root to
    // reduce against, no such path ever matched a repo-relative diff path and
    // every file reported as unmeasured.
    let base = std::env::temp_dir().join(format!("dcv-root-{}", std::process::id()));
    let repo = base.join("repo");
    std::fs::create_dir_all(&repo).unwrap();

    let profile = format!(
        "SF:{}/crates/x/src/lib.rs\nDA:4,1\nend_of_record\n",
        repo.display()
    );
    let parsed = dc_verify::coverage::parse_with_root(&profile, Some(&repo)).expect("must parse");
    assert_eq!(parsed.len(), 1);
    assert_eq!(parsed[0].path, "crates/x/src/lib.rs");

    // The symlinked-ancestor spelling reduces too (macOS /tmp → /private/tmp).
    if let Ok(canonical) = repo.canonicalize() {
        let profile = format!(
            "SF:{}/src/main.rs\nDA:2,1\nend_of_record\n",
            canonical.display()
        );
        let parsed =
            dc_verify::coverage::parse_with_root(&profile, Some(&repo)).expect("must parse");
        assert_eq!(parsed[0].path, "src/main.rs");
    }

    let _ = std::fs::remove_dir_all(&base);
}
