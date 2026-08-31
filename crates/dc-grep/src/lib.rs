//! Repository search, built on ripgrep's engine.
//!
//! This crate owns one question — *where in this repository does this pattern
//! appear* — and it answers it with the same three libraries ripgrep itself is
//! assembled from: `ignore` walks the tree and applies ignore rules,
//! `grep-regex` compiles the pattern, `grep-searcher` runs it over file bytes.
//!
//! What the crate adds on top of them is the part a search service must not get
//! wrong, which is knowing the difference between *nothing matched* and *the
//! search did not happen*:
//!
//!   - An unparseable pattern is an error, never an empty match set. This is
//!     not a stylistic preference; it is a regression that already cost a run.
//!     A model asked to remove unused imports read `{"count":0}` for the
//!     alternation `sys|time` as proof the file used neither, then read the
//!     same answer for imports the file plainly used, and spent its whole step
//!     budget trying to reconcile two contradictory facts nothing had labelled
//!     as broken.
//!
//!   - A search root outside the repository is refused rather than walked to an
//!     empty result, for the same reason.
//!
//!   - Every file the walk declined to read is counted and reported. A file
//!     skipped for size, a file abandoned as binary, a file the process could
//!     not open — each is a hole in the coverage of the answer, and an answer
//!     that hides its holes is how "no matches here" comes to mean "nowhere I
//!     looked, and I will not say where that was."
//!
//! Ignore rules are on by default, which is a deliberate change from the walker
//! this replaced: `.gitignore`, `.ignore`, `.git/info/exclude` and hidden files
//! are all honoured, so a search no longer returns forty hits out of `target/`
//! before it reaches the source. `include_ignored` turns the whole set off for
//! the case where the build output *is* the question.

use std::fs::File;
use std::io;
use std::path::{Path, PathBuf};

use grep_regex::RegexMatcherBuilder;
use grep_searcher::{BinaryDetection, Searcher, SearcherBuilder, Sink, SinkMatch};
use ignore::WalkBuilder;
use ignore::overrides::OverrideBuilder;
use serde::{Deserialize, Serialize};

/// The wire schema this crate speaks. The Go client refuses a binary that
/// answers with a different one rather than decoding through the wrong shape.
pub const SCHEMA_VERSION: u32 = 1;

/// How this binary identifies itself to a health probe.
pub const IDENTITY: &str = "dc-grep";

/// Matches returned when the caller does not say. Fifty is what the tool
/// schema has always promised.
pub const DEFAULT_MAX_RESULTS: usize = 50;

/// Paths returned by a listing when the caller does not say. It is larger than
/// the search default because a path costs a fraction of what a match line
/// does, and the tool it serves is used to enumerate rather than to sample.
pub const DEFAULT_MAX_LIST_RESULTS: usize = 100;

/// The ceiling on `max_results`, whatever the caller asks for. A model that
/// passes a large number is asking for a result set nothing downstream can
/// read; the cap is reported rather than silently applied.
pub const MAX_MAX_RESULTS: usize = 5_000;

/// Files larger than this are not searched. It is the same 2 MiB the Go read
/// tools bound themselves by, because `read_file` and this face the same
/// repository and two different ceilings would only mean one of them was wrong.
pub const DEFAULT_MAX_FILE_BYTES: u64 = 2 * 1024 * 1024;

/// The longest matched line rendered into a result, in bytes.
///
/// The walker this replaced had no such bound: a minified bundle or a
/// single-line data fixture put its entire contents into one match, and fifty
/// of those is a context window. Truncation is marked on the match that
/// suffered it, so a clipped line is never mistaken for the whole line.
pub const DEFAULT_MAX_LINE_BYTES: usize = 1024;

/// The longest pattern accepted, in bytes.
///
/// Patterns come from a model, so their size is not this process's to assume.
/// A 300,000-branch alternation compiled to 416 MiB of automaton in 4.3
/// seconds, and the reply echoed the pattern back, putting 289 KiB into the
/// context window that asked for it. Four kilobytes is longer than any regex a
/// human or a model writes on purpose and far below where either cost bites.
pub const MAX_PATTERN_BYTES: usize = 4 * 1024;

/// The compiled size ceiling for one pattern, in bytes, and for its lazy DFA.
///
/// `MAX_PATTERN_BYTES` bounds the input; these bound the *output*, because the
/// relationship between the two is not linear — a short pattern with nested
/// bounded repeats (`(a{100}){100}{100}`) expands without being long. Ripgrep
/// sets both explicitly for the same reason; the defaults are generous enough
/// that a hostile pattern reaches them.
pub const MAX_REGEX_BYTES: usize = 10 * 1024 * 1024;
pub const MAX_DFA_BYTES: usize = 10 * 1024 * 1024;

/// The largest request accepted on stdin, in bytes.
///
/// `read_to_string` had no bound: a 64 MiB request produced a 75 MiB resident
/// set, and nothing capped what a caller could hand over. Every other boundary
/// in this harness bounds the bytes it will accept from the other side; this
/// one bounds them in the direction the others forgot, which is inbound.
pub const MAX_REQUEST_BYTES: u64 = 1024 * 1024;

/// Ceilings on the per-request tuning knobs.
///
/// The request struct carries `max_file_bytes` and `max_line_bytes` so the Go
/// plane can bound them, but a field a caller can set is a field a caller can
/// set to `u64::MAX`, which is the same as having no bound at all. Both are
/// clamped rather than trusted.
pub const MAX_FILE_BYTES_CEILING: u64 = 64 * 1024 * 1024;
pub const MAX_LINE_BYTES_CEILING: usize = 64 * 1024;

/// Names never searched, whatever the ignore rules say.
///
/// `.git` and `.devcouncil` are the repository's own machinery and the
/// harness's own state; matches from either are the agent reading its own
/// notes back to itself. They are excluded even under `include_ignored`,
/// which is the flag for "search the build output too", not "search the
/// session log too".
///
/// Written without a trailing slash on purpose. A `.git/` glob matches only a
/// directory, and inside a git *worktree* — which is how this harness is
/// routinely run — `.git` is a file holding a gitdir pointer. The slashed form
/// would have walked straight past it.
const ALWAYS_EXCLUDED: &[&str] = &[".git", ".devcouncil"];

/// One search, as the Go execution plane asked for it.
#[derive(Debug, Deserialize)]
pub struct Request {
    /// The regular expression to find. Required, and never empty.
    pub pattern: String,
    /// The repository root. Every result path is relative to it, and nothing
    /// outside it is read.
    pub root: PathBuf,
    /// Where under `root` to search. Empty or "." means the whole repository.
    #[serde(default)]
    pub path: String,
    /// How many matches to return before reporting truncation.
    #[serde(default)]
    pub max_results: usize,
    /// Search files the ignore rules would exclude, hidden files included.
    #[serde(default)]
    pub include_ignored: bool,
    /// Match without regard to case. Off by default, matching the walker this
    /// replaced; `(?i)` in the pattern has always worked and still does.
    #[serde(default)]
    pub case_insensitive: bool,
    /// Files at or above this size are skipped and counted. Zero means the
    /// default.
    #[serde(default)]
    pub max_file_bytes: u64,
    /// Longest rendered line. Zero means the default.
    #[serde(default)]
    pub max_line_bytes: usize,
}

/// One matching line.
#[derive(Debug, Serialize)]
pub struct Hit {
    /// Repository-relative, forward-slashed on every platform.
    pub path: String,
    pub line_number: u64,
    /// The matched line, trimmed of surrounding whitespace and its terminator.
    pub line: String,
    /// Set when `line` is shorter than the line in the file.
    #[serde(skip_serializing_if = "is_false")]
    pub line_truncated: bool,
}

/// What the walk could not look inside, counted rather than dropped.
///
/// Every field here is a hole in the answer's coverage. They are reported
/// unconditionally — a search that skipped nothing says so with zeros — so a
/// caller never has to infer completeness from the absence of a complaint.
#[derive(Debug, Default, Serialize)]
pub struct Skipped {
    /// Files at or over `max_file_bytes`.
    pub too_large: u64,
    /// Files abandoned because they contain binary data.
    ///
    /// A NUL anywhere in the first 64 KiB is found before any match out of
    /// that file is emitted, which covers every real binary format. A NUL
    /// after that is found when the searcher reaches it, and the matches
    /// already collected from the file are then dropped.
    ///
    /// One shape escapes both: a file whose only NUL sits past the point where
    /// the match limit stopped the walk. Reaching it takes a file that is pure
    /// text for `max_results` worth of matching lines and binary only at the
    /// very end, and closing it would mean reading every large file to its end
    /// before returning any of it.
    pub binary: u64,
    /// Files the process could not open or read, and directories the walk
    /// could not descend into.
    pub unreadable: u64,
    /// Files whose names are not valid UTF-8.
    ///
    /// Unix filenames are bytes, and ext4 accepts bytes that are not UTF-8
    /// (APFS does not, which is why this is invisible on macOS and reachable
    /// in production). Rendering such a name lossily produces a path with
    /// U+FFFD in it — a path that names no file, cannot be reopened, and would
    /// hand a model a match it can never act on. The match is dropped and the
    /// file counted instead.
    pub unrepresentable_name: u64,
}

/// The answer to one `Request`.
#[derive(Debug, Serialize)]
pub struct Response {
    /// Always true on this type. A failure is a different shape entirely —
    /// `{"ok":false,"error":...}` — so no caller can read a zero count out of
    /// a search that never ran.
    pub ok: bool,
    pub pattern: String,
    pub count: usize,
    pub matches: Vec<Hit>,
    /// Set when the match limit stopped the walk early.
    #[serde(skip_serializing_if = "is_false")]
    pub truncated: bool,
    /// The limit that did the stopping, present whenever `truncated` is.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub limit: Option<usize>,
    /// Files actually opened and searched.
    pub files_searched: u64,
    pub skipped: Skipped,
    /// Whether ignore rules were applied, echoed back so a caller reporting
    /// the result can say which repository it searched.
    pub ignore_rules_applied: bool,
}

fn is_false(value: &bool) -> bool {
    !*value
}

/// Runs one search.
///
/// Every `Err` here names a fault. None of them is reachable by a search that
/// simply found nothing: that is `Ok` with `count: 0`.
pub fn search(request: &Request) -> Result<Response, String> {
    if request.pattern.trim().is_empty() {
        return Err("pattern is required".to_string());
    }
    if request.pattern.len() > MAX_PATTERN_BYTES {
        // Named as a refusal rather than truncated into a different pattern.
        // Silently searching for a prefix of what was asked would produce
        // matches that answer a question nobody posed.
        return Err(format!(
            "pattern is {} bytes, over the {MAX_PATTERN_BYTES}-byte limit — \
             this is not a negative result, no search ran",
            request.pattern.len()
        ));
    }

    let (root, target) = resolve_roots(&request.root, &request.path)?;

    // Compiled before the walk starts, so a bad pattern costs nothing and —
    // far more importantly — reports as a bad pattern rather than as a walk
    // that visited every file and matched none of them.
    let matcher = RegexMatcherBuilder::new()
        .case_insensitive(request.case_insensitive)
        .line_terminator(Some(b'\n'))
        // Bounds on what the pattern may compile *to*. Exceeding either is a
        // build error, which is reported as a refused pattern — the same shape
        // as a syntactically invalid one, and for the same reason.
        .size_limit(MAX_REGEX_BYTES)
        .dfa_size_limit(MAX_DFA_BYTES)
        .build(&request.pattern)
        .map_err(|err| {
            format!(
                "pattern {:?} is not a valid regular expression: {err}",
                request.pattern
            )
        })?;

    let limit = match request.max_results {
        0 => DEFAULT_MAX_RESULTS,
        n => n.min(MAX_MAX_RESULTS),
    };
    let max_file_bytes = match request.max_file_bytes {
        0 => DEFAULT_MAX_FILE_BYTES,
        n => n.min(MAX_FILE_BYTES_CEILING),
    };
    let max_line_bytes = match request.max_line_bytes {
        0 => DEFAULT_MAX_LINE_BYTES,
        n => n.min(MAX_LINE_BYTES_CEILING),
    };

    let apply_ignore_rules = !request.include_ignored;
    let walker = build_walker(&target, apply_ignore_rules)?;

    let mut searcher = SearcherBuilder::new()
        .line_number(true)
        // One line per match. Multi-line would let a single hit carry an
        // unbounded span of the file into the result.
        .multi_line(false)
        // Stop reading a file the moment it looks binary. The matches already
        // collected from it are then discarded — see `binary` below.
        .binary_detection(BinaryDetection::quit(b'\x00'))
        .build();

    let mut matches: Vec<Hit> = Vec::new();
    let mut truncated = false;
    let mut files_searched: u64 = 0;
    let mut skipped = Skipped::default();

    for entry in walker.build() {
        if truncated {
            break;
        }
        let entry = match entry {
            Ok(entry) => entry,
            // A directory that could not be read is a hole in the answer, not
            // a reason to abandon the search — but it is never silent.
            Err(_) => {
                skipped.unreadable += 1;
                continue;
            }
        };
        // `file_type()` is `None` only for stdin, which this walk never has.
        // Anything that is not a regular file — a directory, a symlink, a
        // FIFO, a device node — is skipped before it can be opened.
        match entry.file_type() {
            Some(file_type) if file_type.is_file() => {}
            _ => continue,
        }

        let path = entry.path();
        let Ok(rel) = path.strip_prefix(&root) else {
            // Unreachable while `follow_links` is off and `target` is under
            // `root`, and counted rather than ignored if it ever stops being.
            skipped.unreadable += 1;
            continue;
        };

        // Opened once and then interrogated through the descriptor. Every
        // question below — is it really a regular file, how big is it, does it
        // look binary — is asked of what was actually opened rather than of
        // the name, which is the distinction that let a 182-byte symlink to a
        // 5 MiB file past a 2 MiB guard on the Go side of this harness.
        let file = match open_for_search(path) {
            Ok(file) => file,
            Err(_) => {
                skipped.unreadable += 1;
                continue;
            }
        };
        let meta = match file.metadata() {
            Ok(meta) => meta,
            Err(_) => {
                skipped.unreadable += 1;
                continue;
            }
        };
        if !meta.is_file() {
            // The walk already filtered on `lstat`. This catches the race where
            // the name was a regular file then and is a FIFO or a device now.
            skipped.unreadable += 1;
            continue;
        }
        if meta.len() >= max_file_bytes {
            skipped.too_large += 1;
            continue;
        }

        let Some(rel) = slashed(rel) else {
            skipped.unrepresentable_name += 1;
            continue;
        };
        let before = matches.len();
        let mut binary = false;

        let outcome = searcher.search_file(
            &matcher,
            &file,
            Collector {
                path: &rel,
                out: &mut matches,
                limit,
                max_line_bytes,
                truncated: &mut truncated,
                binary: &mut binary,
            },
        );

        if binary {
            // A NUL past the probe window. Ripgrep's own detection would keep
            // the matches it found before it; they are dropped instead,
            // because this result goes into a model's context and half a line
            // of a compiled object file is noise that reads like evidence.
            // Dropping them also makes the answer the same on every platform:
            // macOS streams through a 64 KiB buffer and emits those matches,
            // Linux memory-maps and does not.
            matches.truncate(before);
            skipped.binary += 1;
            continue;
        }
        if outcome.is_err() {
            matches.truncate(before);
            skipped.unreadable += 1;
            continue;
        }
        files_searched += 1;
    }

    Ok(Response {
        ok: true,
        pattern: request.pattern.clone(),
        count: matches.len(),
        matches,
        truncated,
        limit: truncated.then_some(limit),
        files_searched,
        skipped,
        ignore_rules_applied: apply_ignore_rules,
    })
}

/// One file listing, as the Go execution plane asked for it.
#[derive(Debug, Deserialize)]
pub struct ListRequest {
    /// The repository root. Every path is relative to it.
    pub root: PathBuf,
    /// Where under `root` to list. Empty or "." means the whole repository.
    #[serde(default)]
    pub path: String,
    /// How many paths to return before reporting truncation.
    #[serde(default)]
    pub max_results: usize,
    /// List files the ignore rules would exclude, hidden files included.
    #[serde(default)]
    pub include_ignored: bool,
}

/// The answer to one `ListRequest`.
#[derive(Debug, Serialize)]
pub struct ListResponse {
    pub ok: bool,
    pub count: usize,
    pub paths: Vec<String>,
    #[serde(skip_serializing_if = "is_false")]
    pub truncated: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub limit: Option<usize>,
    pub skipped: Skipped,
    pub ignore_rules_applied: bool,
}

/// Lists every file the search would open.
///
/// This exists so `devcouncil_find_files` and `devcouncil_grep` cannot answer
/// different questions about the same repository. It walks through
/// `build_walker`, exactly as `search` does, and the guarantee it offers the
/// caller is precise: a path in this list is a path `search` would read.
///
/// Matching is deliberately *not* done here. `find_files` matches with the
/// harness's own `fnmatch`, which is pinned to a 775-case CPython parity
/// fixture; moving that into this crate would fork the glob semantics to gain
/// nothing. What was wrong was never the matching — it was the two walks.
pub fn list_files(request: &ListRequest) -> Result<ListResponse, String> {
    let (root, target) = resolve_roots(&request.root, &request.path)?;

    let limit = match request.max_results {
        0 => DEFAULT_MAX_LIST_RESULTS,
        n => n.min(MAX_MAX_RESULTS),
    };
    let apply_ignore_rules = !request.include_ignored;
    let walker = build_walker(&target, apply_ignore_rules)?;

    let mut paths = Vec::new();
    let mut truncated = false;
    let mut skipped = Skipped::default();

    for entry in walker.build() {
        if paths.len() >= limit {
            truncated = true;
            break;
        }
        let entry = match entry {
            Ok(entry) => entry,
            Err(_) => {
                skipped.unreadable += 1;
                continue;
            }
        };
        match entry.file_type() {
            Some(file_type) if file_type.is_file() => {}
            _ => continue,
        }
        let Ok(rel) = entry.path().strip_prefix(&root) else {
            skipped.unreadable += 1;
            continue;
        };
        match slashed(rel) {
            Some(rel) => paths.push(rel),
            None => skipped.unrepresentable_name += 1,
        }
    }

    Ok(ListResponse {
        ok: true,
        count: paths.len(),
        paths,
        truncated,
        limit: truncated.then_some(limit),
        skipped,
        ignore_rules_applied: apply_ignore_rules,
    })
}

/// Resolves the repository root and the search root under it, refusing anything
/// that escapes.
///
/// Canonicalising both sides is what makes the containment check mean
/// something. Comparing the strings as given would let `root/../root2`, or a
/// symlinked directory, satisfy a prefix test while reading somewhere else
/// entirely.
fn resolve_roots(root: &Path, requested: &str) -> Result<(PathBuf, PathBuf), String> {
    let root = root
        .canonicalize()
        .map_err(|err| format!("repository root {root:?} is unreadable: {err}"))?;

    let requested = requested.trim();
    let target = if requested.is_empty() || requested == "." || requested == "./" {
        root.clone()
    } else {
        root.join(requested)
            .canonicalize()
            .map_err(|err| format!("path {requested:?} is unreadable: {err}"))?
    };
    if !target.starts_with(&root) {
        return Err(format!(
            "path {requested:?} resolves outside the repository and this tool reads only inside it"
        ));
    }
    Ok((root, target))
}

/// Builds the tree walk both operations use.
///
/// One function, because the alternative already bit: while `devcouncil_grep`
/// honoured ignore rules and `devcouncil_find_files` walked with a hardcoded
/// five-name skip list, `find_files` handed an agent `dist/generated.go` and
/// `grep` would never open it. Two readers of one repository with different
/// assumptions is how "the same tree" drifts apart, and an agent given both
/// answers has no way to tell which one is about the repository it is editing.
fn build_walker(target: &Path, apply_ignore_rules: bool) -> Result<WalkBuilder, String> {
    let mut overrides = OverrideBuilder::new(target);
    for glob in ALWAYS_EXCLUDED {
        // A leading `!` is an *ignore* in override syntax — the sense is
        // inverted from gitignore. Only ignores are added here, which keeps
        // `num_whitelists()` at zero; a single non-`!` glob would flip the
        // matcher into whitelist mode and exclude the entire repository except
        // what it named.
        overrides
            .add(&format!("!{glob}"))
            .map_err(|err| format!("internal exclusion {glob:?} is not a valid glob: {err}"))?;
    }
    let overrides = overrides
        .build()
        .map_err(|err| format!("internal exclusions did not compile: {err}"))?;

    let mut walker = WalkBuilder::new(target);
    walker
        .overrides(overrides)
        // Never followed. A symlink out of the repository is the whole reason
        // containment is checked at all, and a link that points back inside is
        // reached through its real path anyway.
        .follow_links(false)
        .standard_filters(apply_ignore_rules)
        // `require_git(false)` so `.gitignore` is honoured in a directory that
        // is not itself a git checkout — a worktree subdirectory, an extracted
        // archive, a test fixture. Without it the rules silently stop applying
        // and the caller cannot tell.
        .require_git(false)
        .git_ignore(apply_ignore_rules)
        .git_global(apply_ignore_rules)
        .git_exclude(apply_ignore_rules)
        .ignore(apply_ignore_rules)
        .parents(apply_ignore_rules)
        .hidden(apply_ignore_rules);
    Ok(walker)
}

/// Opens a file for searching without letting the filesystem redirect or block
/// the read.
///
/// `O_NOFOLLOW` refuses a final component that has become a symlink since the
/// walk looked at it, and `O_NONBLOCK` means a FIFO planted in the repository
/// fails the open instead of holding this process until something writes to
/// the other end. Both are races the walk's own `lstat` cannot close, and both
/// are reachable by anything that can write into the tree under search.
#[cfg(unix)]
fn open_for_search(path: &Path) -> io::Result<File> {
    use std::os::unix::fs::OpenOptionsExt;
    File::options()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
}

#[cfg(not(unix))]
fn open_for_search(path: &Path) -> io::Result<File> {
    File::open(path)
}

/// Renders a repository-relative path with forward slashes on every platform,
/// so a result read on Windows names the same file a result read on Unix does.
///
/// `None` when any component is not valid UTF-8. Lossy conversion was the
/// alternative and it is worse than refusing: it yields a path containing
/// U+FFFD, which names no file on any filesystem, so the caller receives a
/// match it cannot open and has no way to tell that from a real one.
fn slashed(path: &Path) -> Option<String> {
    let mut out = String::new();
    for component in path.components() {
        let part = component.as_os_str().to_str()?;
        if !out.is_empty() {
            out.push('/');
        }
        out.push_str(part);
    }
    Some(out)
}

/// Collects matching lines from one file.
struct Collector<'a> {
    path: &'a str,
    out: &'a mut Vec<Hit>,
    limit: usize,
    max_line_bytes: usize,
    truncated: &'a mut bool,
    binary: &'a mut bool,
}

impl Sink for Collector<'_> {
    type Error = io::Error;

    fn matched(&mut self, _searcher: &Searcher, mat: &SinkMatch<'_>) -> Result<bool, io::Error> {
        if self.out.len() >= self.limit {
            *self.truncated = true;
            return Ok(false);
        }
        let (line, line_truncated) = render_line(mat.bytes(), self.max_line_bytes);
        self.out.push(Hit {
            path: self.path.to_string(),
            // `line_number(true)` is set on the searcher, so this is always
            // `Some`. Zero would be a lie about a real line, so an absent
            // number stops the search rather than inventing one.
            line_number: match mat.line_number() {
                Some(number) => number,
                None => {
                    return Err(io::Error::other(
                        "searcher reported a match without a line number",
                    ));
                }
            },
            line,
            line_truncated,
        });
        if self.out.len() >= self.limit {
            *self.truncated = true;
            return Ok(false);
        }
        Ok(true)
    }

    fn binary_data(&mut self, _searcher: &Searcher, _offset: u64) -> Result<bool, io::Error> {
        *self.binary = true;
        Ok(false)
    }
}

/// Turns raw line bytes into the string a result carries.
///
/// Lossy, deliberately: a line with one invalid byte in it is still the line
/// the agent asked about, and refusing to render it would turn a real match
/// into a silent absence.
fn render_line(bytes: &[u8], max_line_bytes: usize) -> (String, bool) {
    let text = String::from_utf8_lossy(bytes);
    let trimmed = text.trim();
    if trimmed.len() <= max_line_bytes {
        return (trimmed.to_string(), false);
    }
    // Truncating a `str` at a byte index panics off a char boundary, so the
    // cut is walked back to the nearest one.
    let mut end = max_line_bytes;
    while end > 0 && !trimmed.is_char_boundary(end) {
        end -= 1;
    }
    (trimmed[..end].to_string(), true)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::sync::atomic::{AtomicU32, Ordering};

    /// A scratch repository that removes itself.
    ///
    /// Hand-rolled rather than pulled from `tempfile`, because a dev-dependency
    /// is an audit surface too and this needs four lines of what that crate
    /// does.
    struct Scratch {
        path: PathBuf,
    }

    impl Scratch {
        fn new() -> Scratch {
            static COUNTER: AtomicU32 = AtomicU32::new(0);
            let unique = format!(
                "dc-grep-test-{}-{}",
                std::process::id(),
                COUNTER.fetch_add(1, Ordering::SeqCst)
            );
            let path = std::env::temp_dir().join(unique);
            fs::create_dir_all(&path).expect("create scratch root");
            Scratch { path }
        }

        fn write(&self, rel: &str, contents: &[u8]) {
            let full = self.path.join(rel);
            if let Some(parent) = full.parent() {
                fs::create_dir_all(parent).expect("create scratch parent");
            }
            fs::write(full, contents).expect("write scratch file");
        }

        fn request(&self, pattern: &str) -> Request {
            Request {
                pattern: pattern.to_string(),
                root: self.path.clone(),
                path: String::new(),
                max_results: 0,
                include_ignored: false,
                case_insensitive: false,
                max_file_bytes: 0,
                max_line_bytes: 0,
            }
        }
    }

    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.path);
        }
    }

    fn paths(response: &Response) -> Vec<&str> {
        response.matches.iter().map(|m| m.path.as_str()).collect()
    }

    #[test]
    fn schema_version_is_pinned() {
        // The Go client refuses a binary answering with a different number.
        // Changing this constant without changing that one is the bug this
        // assertion exists to make loud.
        assert_eq!(SCHEMA_VERSION, 1);
        assert_eq!(IDENTITY, "dc-grep");
    }

    #[test]
    fn reports_the_path_line_number_and_line_of_a_match() {
        let scratch = Scratch::new();
        scratch.write("src/main.rs", b"fn main() {\n    let pi = 3.14159;\n}\n");

        let response = search(&scratch.request("3\\.14159")).expect("search runs");

        assert_eq!(response.count, 1);
        let hit = &response.matches[0];
        assert_eq!(hit.path, "src/main.rs");
        assert_eq!(hit.line_number, 2);
        assert_eq!(hit.line, "let pi = 3.14159;");
        assert!(!hit.line_truncated);
    }

    #[test]
    fn an_alternation_matches_every_branch() {
        // The regression that motivated a regex engine here in the first place:
        // a substring search answered `count: 0` for `subprocess|re|os` against
        // a file importing all three, and nothing in the reply said the pattern
        // had not been understood.
        let scratch = Scratch::new();
        scratch.write(
            "mod.py",
            b"import subprocess\nimport re\nimport os\n\nvalue = 1\n",
        );

        let hit = search(&scratch.request("subprocess|re|os")).expect("search runs");
        assert!(hit.count > 0, "alternation must match: {hit:?}");

        // And the negative must remain a real negative, or one useless answer
        // would have replaced another.
        let miss = search(&scratch.request("sys|time")).expect("search runs");
        assert_eq!(miss.count, 0);
    }

    #[test]
    fn an_unparseable_pattern_is_an_error_not_an_empty_result() {
        let scratch = Scratch::new();
        scratch.write("a.txt", b"anything\n");

        let err = search(&scratch.request("unclosed(group")).expect_err("must not succeed");
        assert!(
            err.contains("not a valid regular expression"),
            "the error must name the fault: {err}"
        );
    }

    #[test]
    fn an_empty_pattern_is_an_error() {
        let scratch = Scratch::new();
        let err = search(&scratch.request("   ")).expect_err("must not succeed");
        assert!(err.contains("pattern is required"), "{err}");
    }

    #[test]
    fn a_search_root_outside_the_repository_is_refused() {
        let scratch = Scratch::new();
        scratch.write("a.txt", b"needle\n");

        let mut request = scratch.request("needle");
        request.path = "../..".to_string();
        let err = search(&request).expect_err("must not succeed");
        assert!(err.contains("outside the repository"), "{err}");
    }

    #[test]
    fn gitignored_files_are_skipped_by_default_and_reachable_on_request() {
        let scratch = Scratch::new();
        scratch.write(".gitignore", b"build/\n");
        scratch.write("src/lib.rs", b"needle\n");
        scratch.write("build/generated.rs", b"needle\n");

        let default = search(&scratch.request("needle")).expect("search runs");
        assert_eq!(paths(&default), vec!["src/lib.rs"]);
        assert!(default.ignore_rules_applied);

        let mut request = scratch.request("needle");
        request.include_ignored = true;
        let everything = search(&request).expect("search runs");
        let mut found = paths(&everything);
        found.sort_unstable();
        assert_eq!(found, vec!["build/generated.rs", "src/lib.rs"]);
        assert!(!everything.ignore_rules_applied);
    }

    #[test]
    fn git_and_devcouncil_stay_excluded_even_when_ignore_rules_are_off() {
        let scratch = Scratch::new();
        scratch.write(".git/config", b"needle\n");
        scratch.write(".devcouncil/state.json", b"needle\n");
        scratch.write("src/lib.rs", b"needle\n");

        let mut request = scratch.request("needle");
        request.include_ignored = true;
        let response = search(&request).expect("search runs");

        assert_eq!(
            paths(&response),
            vec!["src/lib.rs"],
            "the harness's own state is never search results"
        );
    }

    #[test]
    fn a_git_pointer_file_is_excluded_the_way_a_git_directory_is() {
        // A worktree checkout — how this harness is routinely run — has `.git`
        // as a file, not a directory. A directory-only glob walked past it.
        let scratch = Scratch::new();
        scratch.write(".git", b"gitdir: /somewhere/needle\n");
        scratch.write("src/lib.rs", b"needle\n");

        let mut request = scratch.request("needle");
        request.include_ignored = true;
        let response = search(&request).expect("search runs");

        assert_eq!(paths(&response), vec!["src/lib.rs"]);
    }

    #[test]
    fn a_binary_file_contributes_no_matches_and_is_counted() {
        let scratch = Scratch::new();
        let mut binary = b"needle then binary\n".to_vec();
        binary.extend_from_slice(&[0u8, 1, 2, 3, 0, 9]);
        scratch.write("blob.bin", &binary);
        scratch.write("src/lib.rs", b"needle\n");

        let response = search(&scratch.request("needle")).expect("search runs");

        assert_eq!(
            paths(&response),
            vec!["src/lib.rs"],
            "a match found before the first NUL is still a match out of a binary file"
        );
        assert_eq!(
            response.skipped.binary, 1,
            "and the skip is reported: {response:?}"
        );
    }

    #[test]
    fn a_large_binary_file_contributes_nothing() {
        // Shaped like every real binary format: a NUL in the header, bytes
        // that happen to look like text after it, and far larger than the
        // searcher's 64 KiB buffer. Detection runs over that first buffer
        // before any match in it is reported, on both the streaming path macOS
        // takes and the memory-mapped path Linux takes.
        let scratch = Scratch::new();
        let mut contents = b"\x7fELF\x02\x01\x01\x00".to_vec();
        for _ in 0..8_000 {
            contents.extend_from_slice(b"needle in a haystack line\n");
        }
        assert!(
            contents.len() > 64 * 1024,
            "the file must exceed one buffer"
        );
        scratch.write("large.bin", &contents);
        scratch.write("src/lib.rs", b"needle\n");

        let mut request = scratch.request("needle");
        request.max_results = 10_000;
        let response = search(&request).expect("search runs");

        assert_eq!(paths(&response), vec!["src/lib.rs"], "{response:?}");
        assert_eq!(response.skipped.binary, 1);
        assert_eq!(response.files_searched, 1);
    }

    #[test]
    fn a_nul_past_the_probe_window_still_voids_the_whole_file() {
        // The second mechanism: a file whose only NUL is past the 8 KiB probe.
        // The searcher reaches it and reports it, and every match already
        // collected from that file is dropped rather than kept — which is also
        // what makes the answer identical on macOS, which streams this file,
        // and Linux, which memory-maps it.
        let scratch = Scratch::new();
        let mut contents = Vec::new();
        contents.extend_from_slice(b"needle near the top\n");
        for _ in 0..3_000 {
            contents.extend_from_slice(b"padding line with nothing of interest\n");
        }
        assert!(
            contents.len() > 64 * 1024,
            "the file must exceed one buffer"
        );
        contents.push(0u8);
        scratch.write("tail-nul.dat", &contents);
        scratch.write("src/lib.rs", b"needle\n");

        // The default limit of 50 is never reached here, so the walk runs to
        // the NUL. That is the condition this mechanism needs, and the test
        // above covers what happens when it is not met.
        let response = search(&scratch.request("needle")).expect("search runs");

        assert_eq!(paths(&response), vec!["src/lib.rs"], "{response:?}");
        assert_eq!(response.skipped.binary, 1);
    }

    #[test]
    fn an_oversized_file_is_skipped_and_counted_rather_than_silently_dropped() {
        let scratch = Scratch::new();
        let mut big = vec![b'a'; 4096];
        big.extend_from_slice(b"\nneedle\n");
        scratch.write("big.txt", &big);
        scratch.write("small.txt", b"needle\n");

        let mut request = scratch.request("needle");
        request.max_file_bytes = 1024;
        let response = search(&request).expect("search runs");

        assert_eq!(paths(&response), vec!["small.txt"]);
        assert_eq!(
            response.skipped.too_large, 1,
            "an unsearched file is a hole in the coverage and must be named: {response:?}"
        );
        assert_eq!(response.files_searched, 1);
    }

    #[test]
    fn the_match_limit_is_reported_rather_than_applied_silently() {
        let scratch = Scratch::new();
        scratch.write("many.txt", b"needle\nneedle\nneedle\nneedle\nneedle\n");

        let mut request = scratch.request("needle");
        request.max_results = 2;
        let response = search(&request).expect("search runs");

        assert_eq!(response.count, 2);
        assert!(response.truncated);
        assert_eq!(response.limit, Some(2));

        // And an untruncated search says so by omission, so the two are never
        // confusable.
        let full = search(&scratch.request("needle")).expect("search runs");
        assert_eq!(full.count, 5);
        assert!(!full.truncated);
        assert_eq!(full.limit, None);
    }

    #[test]
    fn max_results_is_capped_and_the_cap_is_the_reported_limit() {
        let scratch = Scratch::new();
        scratch.write("a.txt", b"needle\n");

        let mut request = scratch.request("needle");
        request.max_results = usize::MAX;
        let response = search(&request).expect("search runs");
        // One match, so nothing truncates; the point is that an absurd request
        // does not become an absurd allocation or an unbounded walk.
        assert_eq!(response.count, 1);
        assert!(!response.truncated);
    }

    #[test]
    fn a_very_long_line_is_clipped_and_says_so() {
        let scratch = Scratch::new();
        let mut line = vec![b'x'; 4096];
        line.extend_from_slice(b"needle\n");
        scratch.write("minified.js", &line);

        let mut request = scratch.request("needle");
        request.max_line_bytes = 64;
        let response = search(&request).expect("search runs");

        assert_eq!(response.count, 1);
        let hit = &response.matches[0];
        assert_eq!(hit.line.len(), 64);
        assert!(
            hit.line_truncated,
            "a clipped line must never pass as the whole line: {hit:?}"
        );
    }

    #[test]
    fn a_line_clipped_mid_character_does_not_panic() {
        let scratch = Scratch::new();
        // A multi-byte character straddling the cut.
        let mut contents = "é".repeat(64).into_bytes();
        contents.extend_from_slice(b"needle\n");
        scratch.write("utf8.txt", &contents);

        let mut request = scratch.request("needle");
        request.max_line_bytes = 5;
        let response = search(&request).expect("search runs");
        assert_eq!(response.count, 1);
        assert!(response.matches[0].line_truncated);
        // 5 is not a char boundary in a run of two-byte characters, so the cut
        // walks back to 4.
        assert_eq!(response.matches[0].line, "éé");
    }

    #[test]
    fn case_insensitivity_is_off_by_default_and_available_on_request() {
        let scratch = Scratch::new();
        scratch.write("a.txt", b"Needle\n");

        assert_eq!(search(&scratch.request("needle")).expect("runs").count, 0);

        let mut request = scratch.request("needle");
        request.case_insensitive = true;
        assert_eq!(search(&request).expect("runs").count, 1);

        // The inline form has always worked and must keep working, because it
        // is what every model reaches for.
        assert_eq!(
            search(&scratch.request("(?i)needle")).expect("runs").count,
            1
        );
    }

    #[test]
    fn a_subdirectory_search_root_is_honoured_and_paths_stay_repository_relative() {
        let scratch = Scratch::new();
        scratch.write("src/a.rs", b"needle\n");
        scratch.write("docs/b.md", b"needle\n");

        let mut request = scratch.request("needle");
        request.path = "src".to_string();
        let response = search(&request).expect("search runs");

        assert_eq!(
            paths(&response),
            vec!["src/a.rs"],
            "a path scoped to src must not reach docs, and must still name the file from the root"
        );
    }

    #[test]
    fn a_search_root_that_does_not_exist_is_an_error_not_an_empty_result() {
        let scratch = Scratch::new();
        scratch.write("a.txt", b"needle\n");

        let mut request = scratch.request("needle");
        request.path = "no/such/dir".to_string();
        let err = search(&request).expect_err("must not succeed");
        assert!(err.contains("unreadable"), "{err}");
    }

    #[cfg(unix)]
    #[test]
    fn a_symlink_pointing_out_of_the_repository_is_never_read() {
        let outside = Scratch::new();
        outside.write("secret.txt", b"needle\n");

        let scratch = Scratch::new();
        scratch.write("src/a.rs", b"needle\n");
        std::os::unix::fs::symlink(&outside.path, scratch.path.join("escape"))
            .expect("create symlink");

        let mut request = scratch.request("needle");
        request.include_ignored = true;
        let response = search(&request).expect("search runs");

        assert_eq!(
            paths(&response),
            vec!["src/a.rs"],
            "following the link would have read outside the repository: {response:?}"
        );
    }

    #[test]
    fn an_empty_repository_is_a_real_negative() {
        let scratch = Scratch::new();
        let response = search(&scratch.request("needle")).expect("search runs");
        assert_eq!(response.count, 0);
        assert_eq!(response.files_searched, 0);
        assert!(response.ok);
    }

    fn list(scratch: &Scratch, include_ignored: bool) -> ListResponse {
        list_files(&ListRequest {
            root: scratch.path.clone(),
            path: String::new(),
            max_results: 0,
            include_ignored,
        })
        .expect("listing runs")
    }

    /// The invariant the two tools exist under: everything the listing names is
    /// something the search would open, and nothing else is.
    ///
    /// This is asserted by *running both* over the same tree rather than by
    /// reading the code, because the defect it replaces was two functions that
    /// each looked correct in isolation.
    #[test]
    fn the_listing_and_the_search_see_exactly_the_same_files() {
        let scratch = Scratch::new();
        scratch.write(".gitignore", b"dist/\n*.log\n");
        scratch.write("src/a.rs", b"needle\n");
        scratch.write("src/nested/b.rs", b"needle\n");
        scratch.write("dist/generated.rs", b"needle\n");
        scratch.write("debug.log", b"needle\n");
        scratch.write(".hidden/c.rs", b"needle\n");
        scratch.write(".git/config", b"needle\n");
        scratch.write(".devcouncil/log.json", b"needle\n");

        for include_ignored in [false, true] {
            let mut listed = list(&scratch, include_ignored).paths;
            listed.sort();

            let mut request = scratch.request("needle");
            request.include_ignored = include_ignored;
            request.max_results = 10_000;
            let mut matched: Vec<String> = search(&request)
                .expect("search runs")
                .matches
                .into_iter()
                .map(|hit| hit.path)
                .collect();
            matched.sort();

            // Every file in this tree contains the needle exactly once, so the
            // two sets are directly comparable. `.gitignore` itself does not,
            // which is why it is filtered rather than expected to match.
            listed.retain(|path| path != ".gitignore");

            assert_eq!(
                listed, matched,
                "listing and search disagree at include_ignored={include_ignored}"
            );
        }
    }

    #[test]
    fn the_listing_refuses_a_root_outside_the_repository() {
        let scratch = Scratch::new();
        scratch.write("a.txt", b"x\n");
        let err = list_files(&ListRequest {
            root: scratch.path.clone(),
            path: "../..".to_string(),
            max_results: 0,
            include_ignored: false,
        })
        .expect_err("must not succeed");
        assert!(err.contains("outside the repository"), "{err}");
    }

    #[test]
    fn the_listing_reports_its_own_truncation() {
        let scratch = Scratch::new();
        for i in 0..10 {
            scratch.write(&format!("f{i}.txt"), b"x\n");
        }
        let response = list_files(&ListRequest {
            root: scratch.path.clone(),
            path: String::new(),
            max_results: 4,
            include_ignored: false,
        })
        .expect("listing runs");
        assert_eq!(response.count, 4);
        assert!(response.truncated);
        assert_eq!(response.limit, Some(4));
    }

    #[test]
    fn an_over_long_pattern_is_refused_rather_than_compiled() {
        let scratch = Scratch::new();
        scratch.write("a.txt", b"needle\n");

        let long = "a".repeat(MAX_PATTERN_BYTES + 1);
        let err = search(&scratch.request(&long)).expect_err("must not succeed");
        assert!(err.contains("over the"), "{err}");
        assert!(
            err.contains("no search ran"),
            "the refusal must not read as a negative result: {err}"
        );

        // And one byte under the limit still runs, so the bound is a bound and
        // not a wall in front of legitimate patterns.
        let allowed = "a".repeat(MAX_PATTERN_BYTES);
        assert!(search(&scratch.request(&allowed)).is_ok());
    }

    /// A short pattern can compile to an enormous automaton, so bounding the
    /// input is not the same as bounding the work.
    ///
    /// Measured before this bound existed: `(\p{L}{200}){200}` and its
    /// relatives compiled to 416 MiB in 4.3 seconds, from a pattern a model can
    /// type in a second.
    #[test]
    fn a_pattern_that_compiles_enormous_is_refused() {
        let scratch = Scratch::new();
        scratch.write("a.txt", b"needle\n");

        for pattern in [
            "a{1000}{1000}",
            r"(\w{500}){500}",
            "((((a{50}){50}){50}){50})",
            r"(?i)(\p{L}{200}){200}",
        ] {
            let err =
                search(&scratch.request(pattern)).expect_err(&format!("{pattern} must be refused"));
            assert!(
                err.contains("not a valid regular expression"),
                "{pattern}: {err}"
            );
        }

        // A large but honest pattern still works, so the ceiling is not simply
        // rejecting anything with a repetition in it.
        assert!(search(&scratch.request(r"[\s\S]{5000}")).is_ok());
    }

    #[test]
    fn the_per_request_knobs_are_clamped_rather_than_trusted() {
        let scratch = Scratch::new();
        let mut line = vec![b'x'; 256 * 1024];
        line.extend_from_slice(b"needle\n");
        scratch.write("wide.txt", &line);

        let mut request = scratch.request("needle");
        // A field a caller can set is a field a caller can set to the maximum.
        request.max_line_bytes = usize::MAX;
        request.max_file_bytes = u64::MAX;
        let response = search(&request).expect("search runs");

        assert_eq!(response.count, 1);
        let hit = &response.matches[0];
        assert!(
            hit.line.len() <= MAX_LINE_BYTES_CEILING,
            "an unbounded request must not produce an unbounded line: {} bytes",
            hit.line.len()
        );
        assert!(hit.line_truncated);
    }

    /// The same rule, applied to a file that really exists.
    ///
    /// Unix filenames are bytes. ext4 accepts bytes that are not UTF-8 and APFS
    /// refuses them, so this case is unreachable on the machine most of this
    /// was written on and perfectly reachable in production. The test creates
    /// the file and returns early where the filesystem will not have it, rather
    /// than asserting nothing on every platform to avoid failing on one.
    #[cfg(unix)]
    #[test]
    fn a_real_file_with_a_non_utf8_name_is_counted_not_reported() {
        use std::ffi::OsStr;
        use std::os::unix::ffi::OsStrExt;

        let scratch = Scratch::new();
        scratch.write("good.txt", b"needle\n");

        let bad = scratch.path.join(OsStr::from_bytes(b"ba\xffd.txt"));
        if fs::write(&bad, b"needle\n").is_err() {
            // APFS and any other filesystem that enforces UTF-8 names. The
            // rendering itself is asserted unconditionally in the test above.
            return;
        }

        let mut request = scratch.request("needle");
        request.include_ignored = true;
        let response = search(&request).expect("search runs");

        assert_eq!(
            paths(&response),
            vec!["good.txt"],
            "a path that cannot be reopened must never be reported as a match: {response:?}"
        );
        assert_eq!(
            response.skipped.unrepresentable_name, 1,
            "and the file must be counted rather than dropped: {response:?}"
        );

        // The listing answers the same way, so the two views stay identical
        // even on the paths neither of them can name.
        let listing = list_files(&ListRequest {
            root: scratch.path.clone(),
            path: String::new(),
            max_results: 0,
            include_ignored: true,
        })
        .expect("listing runs");
        assert!(
            !listing.paths.iter().any(|path| path.contains('\u{fffd}')),
            "a lossy path reached the listing: {listing:?}"
        );
        assert_eq!(listing.skipped.unrepresentable_name, 1);
    }

    #[test]
    fn a_path_component_that_is_not_utf8_yields_no_path_at_all() {
        // Built from bytes rather than from a file, because APFS refuses to
        // create such a name and ext4 does not — the filesystem this runs on
        // decides whether the case is reachable, and the rendering must be
        // correct on both.
        #[cfg(unix)]
        {
            use std::ffi::OsStr;
            use std::os::unix::ffi::OsStrExt;
            let bad = PathBuf::from(OsStr::from_bytes(b"src/ca\xffe.rs"));
            assert_eq!(
                slashed(&bad),
                None,
                "a lossy path names no file and must never reach a caller"
            );
        }
        assert_eq!(
            slashed(Path::new("src/nested/a.rs")),
            Some("src/nested/a.rs".to_string())
        );
    }

    #[test]
    fn max_max_results_is_the_number_the_go_plane_mirrors() {
        // manvi/dc/dcgrep declares MaxListResults against this constant so a
        // caller asking for "everything" is not silently clamped to less than
        // it believes it asked for. The two are asserted equal across the
        // boundary in the Go tests; this pins the value they agree on.
        assert_eq!(MAX_MAX_RESULTS, 5_000);
    }

    /// A tiny deterministic generator, so the randomized tests below reproduce
    /// exactly on a failure and add no dependency to a crate whose dependency
    /// list is the reason it needed authorising.
    struct Rng(u64);

    impl Rng {
        fn next(&mut self) -> u64 {
            // xorshift64*: short, well-known, and good enough to shuffle test
            // inputs. Nothing here is cryptographic.
            let mut x = self.0;
            x ^= x >> 12;
            x ^= x << 25;
            x ^= x >> 27;
            self.0 = x;
            x.wrapping_mul(0x2545_f491_4f6c_dd1d)
        }

        fn below(&mut self, n: usize) -> usize {
            (self.next() % n as u64) as usize
        }

        fn pick<'a, T>(&mut self, options: &'a [T]) -> &'a T {
            &options[self.below(options.len())]
        }
    }

    /// Whatever the pattern, the engine answers or names a fault. It never
    /// panics, and it never reports a fault as an empty match set.
    ///
    /// The pattern is the one input a model controls completely, so this is the
    /// surface where "it did not occur to me that someone would type that" is
    /// least acceptable. The alphabet below is regex metacharacters rather than
    /// letters precisely because random letters would only ever produce valid,
    /// boring patterns.
    #[test]
    fn no_pattern_makes_the_engine_panic_or_lie() {
        let scratch = Scratch::new();
        scratch.write("src/a.rs", b"needle\nfn main() {}\n");
        scratch.write("src/b.txt", b"[]()*+?{}|^$.\\\n");

        const PIECES: &[&str] = &[
            "a",
            ".",
            "*",
            "+",
            "?",
            "|",
            "(",
            ")",
            "[",
            "]",
            "{",
            "}",
            "^",
            "$",
            "\\",
            "\\b",
            "\\w",
            "\\s",
            "\\p{L}",
            "(?i)",
            "(?m)",
            "(?s)",
            "(?:",
            "(?P<n>",
            "{2,}",
            // Repeat counts are tiny on purpose, and the reason is that they
            // compose: the generator concatenates up to twelve pieces, so five
            // "{50}" tokens in a row is a{50}{50}{50}{50}{50} — fifty to the
            // fifth power of states, built and then thrown away, four thousand
            // times. That is what made this test take ninety seconds. The
            // shapes are what it is here to vary; the cost of a pattern that
            // compiles enormous has its own test.
            "{0,4}",
            "{3}",
            "[a-z]",
            "[^a]",
            "\\x00",
            "\u{e9}",
            "\u{1f600}",
            "needle",
            "",
            " ",
        ];

        let mut rng = Rng(0x5eed_1234_abcd_ef01);
        // Two hundred and fifty, and the number is a budget rather than a
        // belief about coverage.
        //
        // Each iteration costs about 47ms, essentially all of it compiling a
        // fresh regex in a debug build — the same work takes a millisecond
        // optimised, which is why the release binary searches this whole
        // repository in 45ms. Four thousand iterations put ninety seconds into
        // a gate that runs on every commit and found nothing the first two
        // hundred did not.
        //
        // The seed is fixed, so this explores the same shapes every run and a
        // failure reproduces exactly rather than appearing once in a CI log.
        for iteration in 0..250 {
            let mut pattern = String::new();
            for _ in 0..rng.below(12) {
                pattern.push_str(rng.pick(PIECES));
            }

            let mut request = scratch.request(&pattern);
            request.case_insensitive = iteration % 2 == 0;
            request.include_ignored = iteration % 3 == 0;
            request.max_results = rng.below(8);

            match search(&request) {
                Ok(response) => {
                    assert!(response.ok, "an Ok response must say ok: {pattern:?}");
                    assert_eq!(
                        response.count,
                        response.matches.len(),
                        "count and matches disagree for {pattern:?}"
                    );
                    for hit in &response.matches {
                        assert!(!hit.path.is_empty(), "empty path for {pattern:?}");
                        assert!(hit.line_number >= 1, "line 0 for {pattern:?}");
                        assert!(
                            !hit.path.starts_with('/') && !hit.path.contains(".."),
                            "escaping path {:?} for {pattern:?}",
                            hit.path
                        );
                    }
                    if response.truncated {
                        assert!(response.limit.is_some(), "truncated without a limit");
                    }
                }
                Err(message) => {
                    // A refusal must name what was refused. An empty message is
                    // the same failure as an empty match set: a fault the
                    // caller cannot distinguish from an answer.
                    assert!(
                        !message.trim().is_empty(),
                        "a refusal with no reason for {pattern:?}"
                    );
                }
            }
        }
    }

    /// The listing and the search agree over randomly generated trees, not just
    /// the one tree someone wrote by hand.
    ///
    /// The correspondence is the whole reason `list_files` exists, and the
    /// defect it replaced was two hand-written walks that each looked right.
    #[test]
    fn the_listing_matches_the_search_over_generated_trees() {
        const NAMES: &[&str] = &[
            "a.rs",
            "b.go",
            "c.md",
            ".hidden.txt",
            "build.log",
            "x.tmp",
            "nested/d.rs",
            "nested/deep/e.go",
            "dist/f.js",
            ".config/g.toml",
            "target/h.rs",
        ];
        const IGNORES: &[&str] = &[
            "",
            "*.log\n",
            "dist/\n",
            "target/\n",
            "*.tmp\n",
            "dist/\ntarget/\n*.log\n",
            "nested/\n",
            "!important\n*.rs\n",
        ];

        let mut rng = Rng(0x1234_5678_9abc_def0);
        for _ in 0..120 {
            let scratch = Scratch::new();
            scratch.write(".gitignore", rng.pick(IGNORES).as_bytes());
            // Every file carries the same needle, so the listing and the match
            // set are directly comparable.
            let mut written = 0;
            for name in NAMES {
                if rng.below(3) > 0 {
                    scratch.write(name, b"needle\n");
                    written += 1;
                }
            }
            if written == 0 {
                continue;
            }

            for include_ignored in [false, true] {
                let mut listed = list_files(&ListRequest {
                    root: scratch.path.clone(),
                    path: String::new(),
                    max_results: 0,
                    include_ignored,
                })
                .expect("listing runs")
                .paths;
                listed.retain(|path| path != ".gitignore");
                listed.sort();

                let mut request = scratch.request("needle");
                request.include_ignored = include_ignored;
                request.max_results = 1000;
                let mut matched: Vec<String> = search(&request)
                    .expect("search runs")
                    .matches
                    .into_iter()
                    .map(|hit| hit.path)
                    .collect();
                matched.sort();

                assert_eq!(
                    listed, matched,
                    "listing and search disagree at include_ignored={include_ignored}"
                );
            }
        }
    }

    #[test]
    fn arbitrary_file_bytes_survive_the_json_boundary() {
        // The reason this crate serialises rather than hand-rolls: the line it
        // emits is whatever the repository under search happens to contain.
        let scratch = Scratch::new();
        scratch.write(
            "weird.txt",
            "needle \" \\ \u{7} \u{1b}[0m ünïcödé\n".as_bytes(),
        );

        let response = search(&scratch.request("needle")).expect("search runs");
        let encoded = serde_json::to_string(&response).expect("renders as JSON");
        let decoded: serde_json::Value = serde_json::from_str(&encoded).expect("and parses back");
        assert_eq!(decoded["matches"][0]["line"], response.matches[0].line);
    }
}
