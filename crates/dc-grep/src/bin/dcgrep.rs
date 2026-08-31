//! `dcgrep` — repository search, over a JSON-on-stdio boundary.
//!
//! Same contract as `dcstore` and `dcverify`, for the same reason: the Go
//! execution plane crosses to Rust by process rather than by cgo, so
//! `CGO_ENABLED=0`, simple cross-compilation and the single static binary all
//! survive, and a crash in the search engine cannot take the agent loop with it.
//!
//! It reads one JSON request on stdin and prints one JSON object on stdout.
//! A search it could not run is exit 2 with an error, never an empty match
//! list: an empty list means "this search ran and matched nothing", and no
//! caller may be allowed to read "could not run" as that.

use std::io::Read;
use std::process::ExitCode;

use dc_grep::{IDENTITY, ListRequest, Request, SCHEMA_VERSION};

fn main() -> ExitCode {
    match run() {
        Ok(json) => {
            println!("{json}");
            ExitCode::SUCCESS
        }
        Err(message) => {
            // Hand-rolled rather than serialised, because this is the path
            // taken when serialisation itself is what failed.
            println!(
                "{{\"ok\":false,\"error\":{}}}",
                serde_json::to_string(&message)
                    .unwrap_or_else(|_| "\"error message was not renderable\"".to_string())
            );
            ExitCode::from(2)
        }
    }
}

fn run() -> Result<String, String> {
    let args = collect_args()?;
    match args.first().map(String::as_str) {
        Some("health") => Ok(format!(
            "{{\"ok\":true,\"searcher\":\"{IDENTITY}\",\"schema_version\":{SCHEMA_VERSION}}}"
        )),
        Some("search") | None => search(),
        Some("files") => list(),
        Some(other) => Err(format!("unknown command {other:?} (search, files, health)")),
    }
}

/// Reads one request from stdin, bounded.
///
/// Bounded during the read rather than checked after it. An unbounded
/// `read_to_string` let a 64 MiB request produce a 75 MiB resident set before
/// anything looked at it, and nothing on this side decides how much a caller
/// may send.
///
/// One byte over the limit is taken so the cap can be *detected*: a reader
/// stopped exactly at the limit cannot tell a request that fit from one that
/// was truncated, and a truncated request is very likely still valid JSON with
/// a shorter pattern in it — which would run a search nobody asked for.
fn read_request() -> Result<String, String> {
    let mut raw = String::new();
    let read = std::io::stdin()
        .take(dc_grep::MAX_REQUEST_BYTES + 1)
        .read_to_string(&mut raw)
        .map_err(|err| format!("could not read the request from stdin: {err}"))?;
    if read as u64 > dc_grep::MAX_REQUEST_BYTES {
        return Err(format!(
            "request is larger than the {}-byte limit",
            dc_grep::MAX_REQUEST_BYTES
        ));
    }
    if raw.trim().is_empty() {
        return Err("no request on stdin".to_string());
    }
    Ok(raw)
}

fn search() -> Result<String, String> {
    let raw = read_request()?;
    let request: Request = serde_json::from_str(&raw)
        .map_err(|err| format!("request is not valid JSON for this schema: {err}"))?;
    let response = dc_grep::search(&request)?;
    serde_json::to_string(&response)
        .map_err(|err| format!("result could not be rendered as JSON: {err}"))
}

/// Lists the files a search would open.
///
/// It shares `read_request` with `search` rather than repeating the bound,
/// because a cap that two call sites each implement is a cap one of them will
/// eventually be missing.
fn list() -> Result<String, String> {
    let raw = read_request()?;
    let request: ListRequest = serde_json::from_str(&raw)
        .map_err(|err| format!("request is not valid JSON for this schema: {err}"))?;
    let response = dc_grep::list_files(&request)?;
    serde_json::to_string(&response)
        .map_err(|err| format!("result could not be rendered as JSON: {err}"))
}

/// Collects the command line, refusing an argument that is not valid Unicode
/// rather than dying on it.
///
/// `std::env::args()` panics on such an argument: exit 101, a backtrace on
/// stderr and nothing on stdout, which the Go client can only report as a
/// search that failed with no reason. A search that could not run must stay
/// distinguishable from one that ran.
fn collect_args() -> Result<Vec<String>, String> {
    let mut args = Vec::new();
    for (index, arg) in std::env::args_os().skip(1).enumerate() {
        match arg.into_string() {
            Ok(value) => args.push(value),
            Err(_) => return Err(format!("argument {index} is not valid UTF-8")),
        }
    }
    Ok(args)
}
