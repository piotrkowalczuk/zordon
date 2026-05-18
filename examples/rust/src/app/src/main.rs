// Minimal std-only HTTP server for examples/rust — no crates, so
// `cargo install` is fast and works offline.
use std::io::{Read, Write};
use std::net::TcpListener;

fn main() {
    let addr = std::env::args()
        .nth(2)
        .unwrap_or_else(|| "127.0.0.1:8080".to_string());
    let listener = TcpListener::bind(&addr).expect("bind");
    println!("up {addr}");
    for stream in listener.incoming() {
        let mut s = match stream {
            Ok(s) => s,
            Err(_) => continue,
        };
        let mut buf = [0u8; 1024];
        let _ = s.read(&mut buf);
        let body = "rust-example ok\n";
        let resp = format!(
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        );
        let _ = s.write_all(resp.as_bytes());
    }
}
