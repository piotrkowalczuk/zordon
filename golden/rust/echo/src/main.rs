// echo is a conformance-test fixture: a Rust service that mirrors back
// its execution context (env, cwd, argv, the rustc that built it) as
// JSON so tests can assert on what zordon actually passed it. It is
// the Rust twin of golden/go/echo and speaks the identical wire shape
// so the Go-side test harness decodes both with one struct.
//
// std-only on purpose: no crates means `cargo install` is fast, works
// offline, and the suite never depends on crates.io being reachable.
//
// argv convention matches the Go fixture: `-addr HOST:PORT` selects
// the listen address, `-tag VALUE` is an inert flag some tests pass
// to prove argv interpolation reached the binary (it shows up in the
// echoed `argv`).
use std::io::{Read, Write};
use std::net::TcpListener;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let addr = flag(&args, "-addr").unwrap_or_else(|| "127.0.0.1:0".to_string());

    let listener = match TcpListener::bind(&addr) {
        Ok(l) => l,
        Err(e) => {
            // One-shot daemon: a bind failure is fatal. The test
            // observes connection-refused on GET / and fails with a
            // useful message.
            eprintln!("bind {addr}: {e}");
            std::process::exit(1);
        }
    };
    println!("up {addr}");

    let body = response_body(&args);
    let http = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        body.len(),
        body
    );

    for stream in listener.incoming() {
        let mut s = match stream {
            Ok(s) => s,
            Err(_) => continue,
        };
        let mut buf = [0u8; 1024];
        let _ = s.read(&mut buf);
        let _ = s.write_all(http.as_bytes());
    }
}

// flag returns the value following `name` in argv, or None.
fn flag(args: &[String], name: &str) -> Option<String> {
    args.iter()
        .position(|a| a == name)
        .and_then(|i| args.get(i + 1))
        .cloned()
}

// response_body renders the same JSON object golden/go/echo emits:
// {"env":{..},"cwd":"..","argv":[..],"runtime_version":".."}.
// With the `greeting` cargo feature it adds {"greeting":"on"} so the
// features-reach-build test has an observable signal.
fn response_body(args: &[String]) -> String {
    let cwd = std::env::current_dir()
        .map(|p| p.to_string_lossy().into_owned())
        .unwrap_or_default();

    let mut env_pairs = String::new();
    for (i, (k, v)) in std::env::vars().enumerate() {
        if i > 0 {
            env_pairs.push(',');
        }
        env_pairs.push_str(&format!("{}:{}", json_str(&k), json_str(&v)));
    }

    let argv = args
        .iter()
        .map(|a| json_str(a))
        .collect::<Vec<_>>()
        .join(",");

    let runtime_version = env!("ECHO_RUSTC");

    let mut obj = format!(
        "{{\"env\":{{{}}},\"cwd\":{},\"argv\":[{}],\"runtime_version\":{}",
        env_pairs,
        json_str(&cwd),
        argv,
        json_str(runtime_version),
    );
    if cfg!(feature = "greeting") {
        obj.push_str(",\"greeting\":\"on\"");
    }
    obj.push('}');
    obj
}

// json_str renders a Rust string as a JSON string literal, escaping
// the characters the JSON grammar requires. Env values are arbitrary
// bytes-as-UTF8, so this has to be correct, not just good enough.
fn json_str(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
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
