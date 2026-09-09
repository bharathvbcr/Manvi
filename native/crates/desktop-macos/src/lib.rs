#[cfg(target_os = "macos")]
mod input_guard;
#[cfg(target_os = "macos")]
mod platform;
#[cfg(target_os = "macos")]
pub use platform::execute;
