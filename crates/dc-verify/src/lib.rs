//! The first piece of the deterministic verifier, ported to the analysis plane.
//!
//! Two jobs, both pure CPU work over text, both needing to be exactly right:
//!
//! 1. Parse a unified diff into changed files and the *new-file* line numbers
//!    of added lines. Every downstream gate — orphan-diff detection, the
//!    diff↔coverage intersection, stub and effort heuristics — reads its input
//!    from here, so an error at this layer is invisible everywhere above it.
//!
//! 2. Classify those files against a task's planned scope.
//!
//! The governing rule, inherited from DevCouncil and from the Rust port's own
//! hard-won ledger: **a parse that could not run must never look like a parse
//! that ran and found nothing.** A malformed diff returns `Err`, never an empty
//! vector — an empty vector means "this diff genuinely changed no files", and
//! an orphan check reading a silently-empty parse would report a clean scope
//! for a diff that touched the whole repository.

pub mod coverage;
pub mod rigor;

use std::fmt;

/// How a file was changed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ChangeStatus {
    Added,
    Modified,
    Deleted,
    Renamed,
}

/// One file's changes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileDiff {
    /// Repo-relative path after the change. For a deletion this is the path
    /// that was removed.
    pub path: String,
    /// Previous path, set for renames.
    pub old_path: Option<String>,
    pub status: ChangeStatus,
    /// Added lines as (line number in the new file, content without the '+').
    pub added_lines: Vec<(u32, String)>,
    /// Count of removed lines.
    pub removed_count: u32,
}

impl FileDiff {
    /// Line numbers of added lines, which is what the coverage intersection
    /// needs.
    pub fn added_line_numbers(&self) -> Vec<u32> {
        self.added_lines.iter().map(|(n, _)| *n).collect()
    }
}

/// A diff that could not be parsed. Carries the line so the failure is
/// actionable rather than a bare "invalid".
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParseError {
    pub line_number: usize,
    pub line: String,
    pub reason: &'static str,
}

impl fmt::Display for ParseError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "unified diff parse failed at line {}: {} ({:?})",
            self.line_number, self.reason, self.line
        )
    }
}

impl std::error::Error for ParseError {}

/// Parses a unified (git) diff.
///
/// Returns `Err` on a malformed hunk header or a hunk body line that is not a
/// recognised marker. Both are conditions under which the added-line numbering
/// would be wrong, and wrong line numbers are worse than no line numbers: the
/// coverage gate would intersect against lines that do not exist and report a
/// diff as unexercised when it was tested.
pub fn parse_unified(diff: &str) -> Result<Vec<FileDiff>, ParseError> {
    let mut files: Vec<FileDiff> = Vec::new();
    let mut current: Option<FileDiff> = None;
    let mut new_line: u32 = 0;
    let mut hunk_remaining: u32 = 0;
    let mut pending_rename_from: Option<String> = None;
    let mut saw_content_before_any_file = false;

    for (idx, raw) in diff.lines().enumerate() {
        let lineno = idx + 1;

        if let Some(rest) = raw.strip_prefix("diff --git ") {
            if let Some(done) = current.take() {
                files.push(done);
            }
            hunk_remaining = 0;
            pending_rename_from = None;
            let path = git_header_path(rest).ok_or_else(|| ParseError {
                line_number: lineno,
                line: raw.to_string(),
                reason: "unreadable `diff --git` header",
            })?;
            current = Some(FileDiff {
                path,
                old_path: None,
                status: ChangeStatus::Modified,
                added_lines: Vec::new(),
                removed_count: 0,
            });
            continue;
        }

        let Some(file) = current.as_mut() else {
            // Preamble before the first `diff --git` (commit message, etc.).
            // Tracked rather than skipped, so input that is not a diff at all
            // can be told from a diff that legitimately changed nothing — see
            // the check after the loop.
            if !raw.trim().is_empty() {
                saw_content_before_any_file = true;
            }
            continue;
        };

        if hunk_remaining == 0 {
            if let Some(rest) = raw.strip_prefix("rename from ") {
                pending_rename_from = Some(strip_prefix_marker(rest));
                continue;
            }
            if let Some(rest) = raw.strip_prefix("rename to ") {
                file.status = ChangeStatus::Renamed;
                file.old_path = pending_rename_from.take();
                file.path = strip_prefix_marker(rest);
                continue;
            }
            if raw.starts_with("new file mode") {
                file.status = ChangeStatus::Added;
                continue;
            }
            if raw.starts_with("deleted file mode") {
                file.status = ChangeStatus::Deleted;
                continue;
            }
            if let Some(rest) = raw.strip_prefix("--- ") {
                if rest == "/dev/null" {
                    file.status = ChangeStatus::Added;
                }
                continue;
            }
            if let Some(rest) = raw.strip_prefix("+++ ") {
                if rest == "/dev/null" {
                    file.status = ChangeStatus::Deleted;
                } else {
                    // The +++ path is authoritative for the post-change name.
                    file.path = strip_prefix_marker(rest);
                }
                continue;
            }
        }

        if raw.starts_with("@@") {
            let hunk = parse_hunk_header(raw).ok_or_else(|| ParseError {
                line_number: lineno,
                line: raw.to_string(),
                reason: "malformed hunk header",
            })?;
            new_line = hunk.new_start;
            hunk_remaining = hunk.new_count + hunk.old_count;
            continue;
        }

        if hunk_remaining == 0 {
            // Outside a hunk: index lines, mode lines, binary markers.
            continue;
        }

        match raw.chars().next() {
            Some('+') => {
                file.added_lines.push((new_line, raw[1..].to_string()));
                new_line += 1;
                hunk_remaining = hunk_remaining.saturating_sub(1);
            }
            Some('-') => {
                file.removed_count += 1;
                hunk_remaining = hunk_remaining.saturating_sub(1);
            }
            Some(' ') => {
                new_line += 1;
                hunk_remaining = hunk_remaining.saturating_sub(2);
            }
            Some('\\') => {
                // "\ No newline at end of file" belongs to the preceding line.
            }
            None => {
                // A context line that is entirely empty; git emits these bare.
                new_line += 1;
                hunk_remaining = hunk_remaining.saturating_sub(2);
            }
            Some(_) => {
                return Err(ParseError {
                    line_number: lineno,
                    line: raw.to_string(),
                    reason: "unrecognised line inside a hunk",
                });
            }
        }
    }

    if let Some(done) = current.take() {
        files.push(done);
    }
    // Input that carries content but never announced a file is not an empty
    // diff — it is something that is not a diff. Returning Ok(vec![]) for it
    // would tell the orphan check that nothing changed, which is a clean scope
    // report over content nobody parsed. This is the same rule as the
    // malformed-hunk case, applied to the one path that previously escaped it.
    if files.is_empty() && saw_content_before_any_file {
        return Err(ParseError {
            line_number: 1,
            line: diff.lines().next().unwrap_or_default().to_string(),
            reason: "input contains content but no `diff --git` header; this is not a unified diff",
        });
    }

    Ok(files)
}

struct Hunk {
    new_start: u32,
    new_count: u32,
    old_count: u32,
}

/// Parses `@@ -a,b +c,d @@`. Counts default to 1 when omitted, per the format.
fn parse_hunk_header(line: &str) -> Option<Hunk> {
    let body = line.strip_prefix("@@")?;
    let end = body.find("@@")?;
    let mut parts = body[..end].split_whitespace();

    let old = parts.next()?.strip_prefix('-')?;
    let new = parts.next()?.strip_prefix('+')?;

    let (_, old_count) = split_range(old)?;
    let (new_start, new_count) = split_range(new)?;

    Some(Hunk {
        new_start,
        new_count,
        old_count,
    })
}

fn split_range(spec: &str) -> Option<(u32, u32)> {
    match spec.split_once(',') {
        Some((start, count)) => Some((start.parse().ok()?, count.parse().ok()?)),
        None => Some((spec.parse().ok()?, 1)),
    }
}

/// Extracts the post-change path from a `diff --git a/x b/y` header.
fn git_header_path(rest: &str) -> Option<String> {
    // Paths containing spaces make this ambiguous in general; git quotes those,
    // and the b/ marker is the reliable anchor.
    if let Some(pos) = rest.rfind(" b/") {
        return Some(rest[pos + 3..].trim_matches('"').to_string());
    }
    let mut parts = rest.split_whitespace();
    let _a = parts.next()?;
    let b = parts.next()?;
    Some(strip_prefix_marker(b))
}

/// Strips git's a/ or b/ prefix and any surrounding quotes, and drops a
/// trailing tab-separated timestamp that plain `diff -u` appends.
fn strip_prefix_marker(path: &str) -> String {
    let path = path.split('\t').next().unwrap_or(path).trim();
    let path = path.trim_matches('"');
    for marker in ["a/", "b/"] {
        if let Some(rest) = path.strip_prefix(marker) {
            return rest.to_string();
        }
    }
    path.to_string()
}

/// The result of checking changed files against a task's declared scope.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ScopeReport {
    /// Files covered by a planned entry.
    pub in_scope: Vec<String>,
    /// Files the diff touched that the plan never named. These are the orphan
    /// diffs the verifier blocks on.
    pub orphans: Vec<String>,
    /// Planned files the diff never touched. Not an error on its own, but the
    /// effort heuristics read it.
    pub untouched_planned: Vec<String>,
}

impl ScopeReport {
    pub fn is_clean(&self) -> bool {
        self.orphans.is_empty()
    }
}

/// Classifies changed files against planned-file globs.
///
/// `planned` entries are fnmatch patterns in the Python dialect, matched with
/// `dc_glob` so this agrees with the Go write gate exactly.
pub fn classify_scope(files: &[FileDiff], planned: &[String]) -> ScopeReport {
    let mut report = ScopeReport::default();
    let mut matched_planned = vec![false; planned.len()];

    for file in files {
        let candidate = normalize_candidate(&file.path);
        let mut hit = false;
        for (i, pattern) in planned.iter().enumerate() {
            let pattern = pattern.replace('\\', "/");
            if candidate == pattern || dc_glob::matches(&pattern, &candidate) {
                matched_planned[i] = true;
                hit = true;
            }
        }
        if hit {
            report.in_scope.push(candidate);
        } else {
            report.orphans.push(candidate);
        }
    }

    for (i, pattern) in planned.iter().enumerate() {
        if !matched_planned[i] {
            report.untouched_planned.push(pattern.clone());
        }
    }

    report.in_scope.sort();
    report.orphans.sort();
    report.untouched_planned.sort();
    report
}

/// Strips the leading `./` the way the write gate does, so both surfaces judge
/// the same string.
pub fn normalize_candidate(path: &str) -> String {
    let mut p = path.replace('\\', "/");
    while let Some(rest) = p.strip_prefix("./") {
        p = rest.to_string();
    }
    p
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = "\
diff --git a/src/calc.go b/src/calc.go
index 1234567..89abcde 100644
--- a/src/calc.go
+++ b/src/calc.go
@@ -10,6 +10,8 @@ func Add(a, b int) int {
 	return a + b
 }

+func Sub(a, b int) int {
+	return a - b
+}

 func unchanged() {}
diff --git a/README.md b/README.md
new file mode 100644
--- /dev/null
+++ b/README.md
@@ -0,0 +1,2 @@
+# Title
+body
";

    #[test]
    fn parses_files_statuses_and_added_line_numbers() {
        let files = parse_unified(SAMPLE).expect("parse");
        assert_eq!(files.len(), 2);

        assert_eq!(files[0].path, "src/calc.go");
        assert_eq!(files[0].status, ChangeStatus::Modified);
        // The three added lines start after two context lines from line 10.
        assert_eq!(files[0].added_line_numbers(), vec![13, 14, 15]);
        assert_eq!(files[0].added_lines[0].1, "func Sub(a, b int) int {");

        assert_eq!(files[1].path, "README.md");
        assert_eq!(files[1].status, ChangeStatus::Added);
        assert_eq!(files[1].added_line_numbers(), vec![1, 2]);
    }

    #[test]
    fn deletion_is_recognised() {
        let diff = "\
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-one
-two
";
        let files = parse_unified(diff).expect("parse");
        assert_eq!(files.len(), 1);
        assert_eq!(files[0].status, ChangeStatus::Deleted);
        assert_eq!(files[0].removed_count, 2);
        assert!(files[0].added_lines.is_empty());
    }

    #[test]
    fn rename_keeps_both_paths() {
        let diff = "\
diff --git a/old/name.rs b/new/name.rs
similarity index 96%
rename from old/name.rs
rename to new/name.rs
";
        let files = parse_unified(diff).expect("parse");
        assert_eq!(files[0].status, ChangeStatus::Renamed);
        assert_eq!(files[0].path, "new/name.rs");
        assert_eq!(files[0].old_path.as_deref(), Some("old/name.rs"));
    }

    #[test]
    fn hunk_without_counts_defaults_to_one() {
        let diff = "\
diff --git a/x.txt b/x.txt
--- a/x.txt
+++ b/x.txt
@@ -3 +3 @@
-old
+new
";
        let files = parse_unified(diff).expect("parse");
        assert_eq!(files[0].added_line_numbers(), vec![3]);
        assert_eq!(files[0].removed_count, 1);
    }

    /// The rule the module exists to hold: a diff that cannot be parsed is an
    /// error, not an empty result that reads as "nothing changed".
    #[test]
    fn malformed_hunk_header_is_an_error_not_an_empty_parse() {
        let diff = "\
diff --git a/x.txt b/x.txt
--- a/x.txt
+++ b/x.txt
@@ this is not a hunk header @@
+something
";
        let err = parse_unified(diff).expect_err("must not parse");
        assert_eq!(err.reason, "malformed hunk header");
        assert_eq!(err.line_number, 4);
    }

    #[test]
    fn garbage_inside_a_hunk_is_an_error() {
        let diff = "\
diff --git a/x.txt b/x.txt
--- a/x.txt
+++ b/x.txt
@@ -1,1 +1,2 @@
 context
!corrupt
";
        let err = parse_unified(diff).expect_err("must not parse");
        assert_eq!(err.reason, "unrecognised line inside a hunk");
    }

    #[test]
    fn an_empty_diff_is_genuinely_empty() {
        assert_eq!(parse_unified("").expect("parse"), vec![]);
    }

    #[test]
    fn scope_classification_finds_orphans() {
        let files = parse_unified(SAMPLE).expect("parse");
        let planned = vec!["src/calc.go".to_string()];
        let report = classify_scope(&files, &planned);

        assert_eq!(report.in_scope, vec!["src/calc.go"]);
        assert_eq!(report.orphans, vec!["README.md"]);
        assert!(report.untouched_planned.is_empty());
        assert!(!report.is_clean());
    }

    #[test]
    fn scope_globs_use_python_semantics() {
        let files = parse_unified(SAMPLE).expect("parse");
        // "src/*" must cover "src/calc.go" — and under shell globbing it would
        // also have to cover nested paths, which is the behaviour dc-glob keeps.
        let planned = vec!["src/*".to_string(), "*.md".to_string()];
        let report = classify_scope(&files, &planned);
        assert!(
            report.is_clean(),
            "unexpected orphans: {:?}",
            report.orphans
        );
    }

    #[test]
    fn untouched_planned_files_are_reported() {
        let files = parse_unified(SAMPLE).expect("parse");
        let planned = vec![
            "src/calc.go".to_string(),
            "README.md".to_string(),
            "src/never_touched.go".to_string(),
        ];
        let report = classify_scope(&files, &planned);
        assert!(report.is_clean());
        assert_eq!(report.untouched_planned, vec!["src/never_touched.go"]);
    }

    /// Ground truth from DevCouncil's own `normalize_planned_candidate`, which
    /// strips "./" in a loop. The third case is the one worth pinning: a
    /// doubled slash leaves a leading "/" behind rather than being cleaned
    /// away, and both ports must reproduce that rather than "improving" it —
    /// a path that normalises differently here than in the write gate is a
    /// path the two surfaces disagree about.
    #[test]
    fn leading_dot_slash_matches_the_python_original() {
        assert_eq!(normalize_candidate("./src/a.go"), "src/a.go");
        assert_eq!(normalize_candidate("././x"), "x");
        assert_eq!(normalize_candidate(".//src/a.go"), "/src/a.go");
        assert_eq!(normalize_candidate("src/a.go"), "src/a.go");
    }
}
