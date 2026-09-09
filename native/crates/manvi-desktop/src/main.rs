mod broker;
mod platform;
fn main() {
    if let Err(error) = broker::run() {
        eprintln!("manvi-desktop: {error}");
        std::process::exit(1);
    }
}
