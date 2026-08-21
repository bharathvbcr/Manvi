//! Rigor gates: the checks that read the *content* of a diff rather than its
//! shape.
//!
//! Scope classification answers "did this task change files it was allowed to
//! change". These answer the harder question: "is what it wrote actually the
//! work". They are the three gates the Go verifier previously listed in its
//! degraded set, which is why a passing verification could not be trusted to
//! mean very much.
//!
//! Every finding carries a file, a line, and the text that triggered it, so a
//! report is actionable rather than a verdict. And every gate is deliberately
//! conservative about what it flags: a rigor check with a high false-positive
//! rate gets turned off, and a gate nobody runs protects nothing.

use crate::FileDiff;

/// How serious a finding is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Severity {
    /// Blocks the task. Reserved for findings where shipping is clearly wrong.
    Blocking,
    /// Reported and does not block.
    Advisory,
}

/// One thing a gate found.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Finding {
    pub gate: &'static str,
    pub severity: Severity,
    pub path: String,
    pub line: u32,
    /// What triggered it. Truncated, and never the full line for a secret
    /// finding — see `redact`.
    pub evidence: String,
    pub message: String,
}

/// Placeholder markers. Matched case-insensitively against added lines only:
/// an existing TODO in untouched code is somebody else's decision, and
/// flagging it would make every diff in a legacy file fail.
const STUB_MARKERS: &[&str] = &[
    "todo",
    "fixme",
    "xxx:",
    "hack:",
    "not implemented",
    "notimplemented",
    "unimplemented",
    "placeholder",
    "for now",
    "stub",
];

/// Bodies that do nothing, in the languages this repo builds.
const EMPTY_BODIES: &[&str] = &[
    "todo!()",
    "unimplemented!()",
    "panic!(\"todo\")",
    "raise notimplementederror",
    "pass  # todo",
];

/// Runs the stub and effort heuristics over a diff.
///
/// Only added lines are read. A gate that also read removed lines would flag a
/// diff for *deleting* a TODO, which is the opposite of what it is for.
pub fn detect_stubs(files: &[FileDiff]) -> Vec<Finding> {
    let mut findings = Vec::new();
    for file in files {
        for (line_no, content) in &file.added_lines {
            let lowered = content.to_ascii_lowercase();
            let trimmed = lowered.trim();

            if let Some(marker) = EMPTY_BODIES.iter().find(|m| trimmed.contains(**m)) {
                findings.push(Finding {
                    gate: "stub_detection",
                    severity: Severity::Blocking,
                    path: file.path.clone(),
                    line: *line_no,
                    evidence: truncate(content.trim(), 120),
                    message: format!(
                        "added code whose body is `{marker}`; the task is not implemented"
                    ),
                });
                continue;
            }

            // Comment markers are advisory. A TODO in a new file is often a
            // legitimate note about future work, and blocking on it would make
            // the gate something people route around.
            if let Some(marker) = STUB_MARKERS.iter().find(|m| trimmed.contains(**m)) {
                if !is_comment_or_string(trimmed) {
                    continue;
                }
                findings.push(Finding {
                    gate: "stub_detection",
                    severity: Severity::Advisory,
                    path: file.path.clone(),
                    line: *line_no,
                    evidence: truncate(content.trim(), 120),
                    message: format!("added a `{marker}` marker"),
                });
            }
        }
    }
    findings
}

/// Reports whether a line looks like a comment or a string literal, which is
/// where a placeholder marker means something.
///
/// The alternative — flagging the marker anywhere — makes an identifier such as
/// `todoItems` or a legitimate `stubServer` into a finding, and a gate that
/// fires on ordinary names is a gate that gets disabled.
fn is_comment_or_string(trimmed: &str) -> bool {
    trimmed.starts_with("//")
        || trimmed.starts_with('#')
        || trimmed.starts_with("/*")
        || trimmed.starts_with('*')
        || trimmed.starts_with("--")
        || trimmed.starts_with("<!--")
        || trimmed.contains("\" todo")
        || trimmed.contains("// todo")
}

/// A credential shape worth stopping a commit for.
struct SecretPattern {
    name: &'static str,
    /// Literal prefix the value starts with.
    prefix: &'static str,
    /// Minimum length of the whole token, including the prefix.
    min_len: usize,
}

/// Vendor-prefixed key shapes.
///
/// Matching on a documented prefix plus a length floor, rather than on entropy,
/// is the deliberate choice here. Entropy scoring flags base64 blobs, minified
/// assets, test fixtures, and hashes — and a secret scanner that cries wolf is
/// one whose findings get waved through, which is strictly worse than not
/// having it. These prefixes are published by their vendors and do not occur by
/// accident.
const SECRET_PATTERNS: &[SecretPattern] = &[
    SecretPattern {
        name: "anthropic api key",
        prefix: "sk-ant-",
        min_len: 24,
    },
    SecretPattern {
        name: "openai api key",
        prefix: "sk-proj-",
        min_len: 24,
    },
    SecretPattern {
        name: "xai api key",
        prefix: "xai-",
        min_len: 20,
    },
    SecretPattern {
        name: "google api key",
        prefix: "AIza",
        min_len: 30,
    },
    SecretPattern {
        name: "github token",
        prefix: "ghp_",
        min_len: 30,
    },
    SecretPattern {
        name: "github pat",
        prefix: "github_pat_",
        min_len: 40,
    },
    SecretPattern {
        name: "slack token",
        prefix: "xoxb-",
        min_len: 30,
    },
    SecretPattern {
        name: "stripe live key",
        prefix: "sk_live_",
        min_len: 24,
    },
    SecretPattern {
        name: "aws access key id",
        prefix: "AKIA",
        min_len: 20,
    },
    SecretPattern {
        name: "private key block",
        prefix: "-----BEGIN",
        min_len: 20,
    },
];

/// Scans added lines for credential shapes.
///
/// This is the gate whose *absence* mattered most. The write gate refuses to
/// write to `.env`, but nothing stopped a key from being pasted into an
/// ordinary source file — and a credential committed to history is compromised
/// even after it is deleted, because the object stays in the repository.
pub fn scan_secrets(files: &[FileDiff]) -> Vec<Finding> {
    let mut findings = Vec::new();
    for file in files {
        for (line_no, content) in &file.added_lines {
            for pattern in SECRET_PATTERNS {
                let Some(start) = content.find(pattern.prefix) else {
                    continue;
                };
                let token: String = content[start..]
                    .chars()
                    .take_while(|c| !c.is_whitespace() && *c != '"' && *c != '\'' && *c != ',')
                    .collect();
                if token.len() < pattern.min_len && !pattern.prefix.starts_with("-----") {
                    continue;
                }
                findings.push(Finding {
                    gate: "secret_scan",
                    severity: Severity::Blocking,
                    path: file.path.clone(),
                    line: *line_no,
                    // The finding names the shape and shows only the prefix.
                    // A report that quotes the key in full copies the secret
                    // into the evidence trail, the terminal, and the session
                    // log — which is the leak this gate exists to prevent.
                    evidence: redact(&token, pattern.prefix.len()),
                    message: format!(
                        "added line contains what looks like a {}; a credential in a commit is \
                         compromised even after it is deleted, because the object stays in history",
                        pattern.name
                    ),
                });
                break;
            }
        }
    }
    findings
}

/// redact keeps the identifying prefix and hides the rest.
fn redact(token: &str, keep: usize) -> String {
    let keep = keep.min(token.len());
    format!("{}… ({} chars)", &token[..keep], token.len())
}

/// A file's test coverage, as a set of covered line numbers.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileCoverage {
    pub path: String,
    pub covered_lines: Vec<u32>,
}

/// What the coverage intersection found for one file.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CoverageGap {
    pub path: String,
    /// Added lines no test executed.
    pub uncovered_lines: Vec<u32>,
    pub added_lines: usize,
}

/// The result of intersecting a diff with coverage data.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct CoverageReport {
    pub gaps: Vec<CoverageGap>,
    /// Files that changed but for which no coverage data was supplied at all.
    ///
    /// This list is the reason the whole report exists in this shape. A file
    /// with no coverage data and a file with full coverage both produce zero
    /// gaps, and reporting them the same way is how "diff coverage passed"
    /// comes to mean "coverage was never measured".
    pub unmeasured: Vec<String>,
}

impl CoverageReport {
    /// Reports whether every added line was measured and executed.
    pub fn is_clean(&self) -> bool {
        self.gaps.is_empty() && self.unmeasured.is_empty()
    }
}

/// Intersects added lines with covered lines.
///
/// Files whose extension marks them as non-executable — documentation, data,
/// configuration — are skipped rather than reported unmeasured, because
/// "coverage" is not a question about a Markdown file.
pub fn intersect_coverage(files: &[FileDiff], coverage: &[FileCoverage]) -> CoverageReport {
    let mut report = CoverageReport::default();
    for file in files {
        if file.status == crate::ChangeStatus::Deleted || !is_executable_source(&file.path) {
            continue;
        }
        let added = file.added_line_numbers();
        if added.is_empty() {
            continue;
        }
        let Some(entry) = coverage.iter().find(|c| c.path == file.path) else {
            report.unmeasured.push(file.path.clone());
            continue;
        };
        let uncovered: Vec<u32> = added
            .iter()
            .copied()
            .filter(|line| !entry.covered_lines.contains(line))
            .collect();
        if !uncovered.is_empty() {
            report.gaps.push(CoverageGap {
                path: file.path.clone(),
                uncovered_lines: uncovered,
                added_lines: added.len(),
            });
        }
    }
    report.unmeasured.sort();
    report.gaps.sort_by(|a, b| a.path.cmp(&b.path));
    report
}

/// Extensions whose files execute, and therefore can be covered.
fn is_executable_source(path: &str) -> bool {
    const EXECUTABLE: &[&str] = &[
        ".go", ".rs", ".py", ".ts", ".tsx", ".js", ".jsx", ".java", ".rb", ".c", ".cc", ".cpp",
        ".h",
    ];
    // A test file's own lines are not the thing coverage is asking about.
    if path.ends_with("_test.go") || path.ends_with("_test.py") || path.contains("/tests/") {
        return false;
    }
    EXECUTABLE.iter().any(|ext| path.ends_with(ext))
}

fn truncate(text: &str, n: usize) -> String {
    if text.chars().count() <= n {
        return text.to_string();
    }
    let cut: String = text.chars().take(n).collect();
    format!("{cut}…")
}
