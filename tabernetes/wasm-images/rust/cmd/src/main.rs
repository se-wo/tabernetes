//! A command component (CO1, CO5, CO6, CO18, FS1 to FS3, AC2). It writes its
//! arguments, its environment, the file in $FILE and its first line of stdin (if
//! $STDIN is set). It writes the file in $WRITE and first shows its old text. It looks
//! up the name in $LOOKUP. It does CPU work for $BUSY_MS milliseconds and
//! fills $ALLOC_MB MiB of memory. Then it exits with the code in $EXIT_CODE.
fn main() {
    for a in std::env::args() {
        println!("arg {a}");
    }
    let mut vars: Vec<_> = std::env::vars().collect();
    vars.sort();
    for (k, v) in vars {
        println!("env {k}={v}");
    }
    if let Ok(f) = std::env::var("FILE") {
        match std::fs::read_to_string(&f) {
            Ok(s) => println!("file {f}: {}", s.trim_end()),
            Err(e) => println!("file {f}: error {e}"),
        }
    }
    if let Ok(w) = std::env::var("WRITE") {
        match std::fs::read_to_string(&w) {
            Ok(s) => println!("old {w}: {}", s.trim_end()),
            Err(e) => println!("old {w}: error {e}"),
        }
        let r = std::fs::write(&w, "written\n").map_err(|e| e.to_string());
        println!("write {w}: {r:?}");
    }
    if let Ok(name) = std::env::var("LOOKUP") {
        use std::net::ToSocketAddrs;
        match (name.as_str(), 80).to_socket_addrs() {
            Ok(a) => println!("lookup {name}: {:?}", a.map(|a| a.ip()).collect::<Vec<_>>()),
            Err(e) => println!("lookup {name}: error {e}"),
        }
    }
    if let Some(ms) = std::env::var("BUSY_MS").ok().and_then(|v| v.parse::<u64>().ok()) {
        // A loop of CPU work that reads the clock (an import of the component).
        let end = std::time::Instant::now() + std::time::Duration::from_millis(ms);
        let mut x: u64 = 1;
        while std::time::Instant::now() < end {
            for i in 0..100_000u64 {
                x = x.wrapping_mul(6364136223846793005).wrapping_add(i);
            }
        }
        println!("busy {ms} ms: {}", x & 1);
    }
    if let Some(mb) = std::env::var("ALLOC_MB").ok().and_then(|v| v.parse::<usize>().ok()) {
        let v = vec![1u8; mb << 20];
        println!("alloc {mb} MiB: {}", v.iter().map(|&b| b as u64).sum::<u64>() >> 20);
    }
    if std::env::var("STDIN").is_ok() {
        // One line, not up to the end: an attach does not close stdin.
        let mut s = String::new();
        let _ = std::io::stdin().read_line(&mut s);
        println!("stdin {}", s.trim_end());
    }
    let code: i32 = std::env::var("EXIT_CODE")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);
    eprintln!("exit {code}");
    std::process::exit(code);
}
