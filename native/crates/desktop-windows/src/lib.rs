#[cfg(target_os = "windows")]
mod platform;
#[cfg(target_os = "windows")]
pub use platform::execute;
