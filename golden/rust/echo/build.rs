// Capture the rustc version at build time so the running binary can
// echo it back as `runtime_version`. Cargo sets $RUSTC for build
// scripts; under zordon that resolves to the mise-pinned toolchain,
// so the conformance suite can assert "this was built by the version
// the Alphasfile pinned" — the Rust analogue of Go's runtime.Version().
use std::process::Command;

fn main() {
    let rustc = std::env::var("RUSTC").unwrap_or_else(|_| "rustc".to_string());
    let version = Command::new(rustc)
        .arg("--version")
        .output()
        .ok()
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| s.trim().to_string())
        .unwrap_or_default();
    println!("cargo:rustc-env=ECHO_RUSTC={version}");
}
