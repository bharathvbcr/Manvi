//! `dcverify` — the analysis plane's verifier, over a JSON-on-stdio boundary.
//!
//! Same contract as `dcstore`, for the same reason: the Go execution plane
//! crosses to Rust by process rather than by cgo, so `CGO_ENABLED=0`, simple
//! cross-compilation, and the single static binary all survive.
//!
//! It reads a unified diff on stdin and prints one JSON object on stdout.
//! Crucially, a diff it cannot parse is exit 2 with an error, never an empty
//! result: an empty finding list means "these gates ran and found nothing", and
//! a caller cannot be allowed to read "could not run" as that.

use std::io::Read;
use std::process::ExitCode;

use dc_verify::rigor::{Finding, Severity, detect_stubs, intersect_coverage, scan_secrets};
use dc_verify::{classify_scope, parse_unified};

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match run(&args) {
        Ok(json) => {
            println!("{json}");
            ExitCode::SUCCESS
        }
        Err(message) => {
            println!("{{\"ok\":false,\"error\":{}}}", quote(&message));
            ExitCode::from(2)
        }
    }
}

const KNOWN_FLAGS: &[&str] = &["planned", "coverage", "root"];
const IDENTITY: &str = "dc-verify";
const SCHEMA_VERSION: u32 = 1;

fn run(args: &[String]) -> Result<String, String> {
    let mut planned: Vec<String> = Vec::new();
    let mut coverage_path: Option<String> = None;
    let mut root: Option<String> = None;
    let mut positional: Vec<&str> = Vec::new();

    let mut i = 0;
    while i < args.len() {
        let arg = args[i].as_str();
        if let Some(name) = arg.strip_prefix("--") {
            if !KNOWN_FLAGS.contains(&name) {
                return Err(format!(
                    "unknown flag --{name} (known: {})",
                    KNOWN_FLAGS.join(", ")
                ));
            }
            let value = args
                .get(i + 1)
                .ok_or_else(|| format!("flag --{name} needs a value"))?;
            // Planned paths arrive newline-separated so a path containing a
            // comma or a space survives the boundary intact.
            if name == "coverage" {
                coverage_path = Some(value.clone());
            } else if name == "root" {
                // Absolute LCOV paths reduce against this; without it the
                // working directory is used, which a host driving the binary
                // from elsewhere cannot rely on.
                root = Some(value.clone());
            } else {
                planned = value
                    .lines()
                    .map(str::trim)
                    .filter(|p| !p.is_empty())
                    .map(str::to_string)
                    .collect();
            }
            i += 2;
        } else {
            positional.push(arg);
            i += 1;
        }
    }

    match positional.first().copied() {
        Some("health") => Ok(format!(
            "{{\"ok\":true,\"verifier\":{},\"schema_version\":{}}}",
            quote(IDENTITY),
            SCHEMA_VERSION
        )),
        Some("check") | None => check(
            &planned,
            coverage_path.as_deref(),
            root.as_deref().map(std::path::Path::new),
        ),
        Some(other) => Err(format!("unknown command {other:?} (check, health)")),
    }
}

fn check(
    planned: &[String],
    coverage_path: Option<&str>,
    root: Option<&std::path::Path>,
) -> Result<String, String> {
    let mut diff = String::new();
    std::io::stdin()
        .read_to_string(&mut diff)
        .map_err(|e| format!("reading the diff from stdin: {e}"))?;

    let files = parse_unified(&diff).map_err(|e| {
        format!(
            "the diff could not be parsed at line {}: {} ({:?})",
            e.line_number, e.reason, e.line
        )
    })?;

    let scope = classify_scope(&files, planned);
    let mut findings = scan_secrets(&files);
    findings.extend(detect_stubs(&files));

    // Coverage measurements, when the caller supplied them. A missing file is
    // fatal rather than silently treated as "no coverage": a caller that asked
    // for coverage and got "everything unmeasured" because of a typo'd path
    // would read the same report as one whose tests genuinely ran nothing.
    let measurements = match coverage_path {
        None => Vec::new(),
        Some(path) => {
            let raw = std::fs::read_to_string(path)
                .map_err(|e| format!("reading the coverage file {path}: {e}"))?;
            dc_verify::coverage::parse_with_root(&raw, root)
                .map_err(|e| format!("parsing the coverage file {path}: {e}"))?
        }
    };
    let coverage = intersect_coverage(&files, &measurements);

    Ok(format!(
        "{{\"ok\":true,\"files\":{},\"in_scope\":{},\"orphans\":{},\"untouched_planned\":{},\
         \"findings\":{},\"coverage_unmeasured\":{},\"coverage_gaps\":{},\
         \"coverage_skipped_by_type\":{}}}",
        files.len(),
        string_array(&scope.in_scope),
        string_array(&scope.orphans),
        string_array(&scope.untouched_planned),
        findings_array(&findings),
        string_array(&coverage.unmeasured),
        coverage_gaps(&coverage),
        // Reported even though nothing blocks on it. A file the gate did not
        // ask about used to leave the reply with no trace, which reads exactly
        // like a file that was measured and clean.
        string_array(&coverage.skipped_by_type),
    ))
}

fn findings_array(findings: &[Finding]) -> String {
    let items: Vec<String> = findings
        .iter()
        .map(|f| {
            format!(
                "{{\"gate\":{},\"severity\":{},\"path\":{},\"line\":{},\"evidence\":{},\"message\":{}}}",
                quote(f.gate),
                quote(match f.severity {
                    Severity::Blocking => "blocking",
                    Severity::Advisory => "advisory",
                }),
                quote(&f.path),
                f.line,
                quote(&f.evidence),
                quote(&f.message),
            )
        })
        .collect();
    format!("[{}]", items.join(","))
}

fn coverage_gaps(report: &dc_verify::rigor::CoverageReport) -> String {
    let items: Vec<String> = report
        .gaps
        .iter()
        .map(|g| {
            let lines: Vec<String> = g.uncovered_lines.iter().map(|n| n.to_string()).collect();
            format!(
                "{{\"path\":{},\"added_lines\":{},\"uncovered_lines\":[{}]}}",
                quote(&g.path),
                g.added_lines,
                lines.join(",")
            )
        })
        .collect();
    format!("[{}]", items.join(","))
}

fn string_array(values: &[String]) -> String {
    let items: Vec<String> = values.iter().map(|v| quote(v)).collect();
    format!("[{}]", items.join(","))
}

/// Hand-rolled JSON string escaping, matching `dcstore`'s. The dependency-free
/// stance is deliberate: these binaries ship inside the harness, and a
/// serialisation crate is a supply-chain surface for output this simple.
fn quote(value: &str) -> String {
    let mut out = String::with_capacity(value.len() + 2);
    out.push('"');
    for c in value.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}
