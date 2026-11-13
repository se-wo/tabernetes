//! A command component: it writes its arguments, its environment, the
//! file in $FILE and stdin, then exits with the code in $EXIT_CODE.
use std::io::Read;

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
        let w = format!("{f}.written");
        println!("write {w}: {:?}", std::fs::write(&w, "x").map_err(|e| e.to_string()));
    }
    if std::env::var("STDIN").is_ok() {
        let mut s = String::new();
        let _ = std::io::stdin().read_to_string(&mut s);
        println!("stdin {}", s.trim_end());
    }
    let code: i32 = std::env::var("EXIT_CODE").ok().and_then(|v| v.parse().ok()).unwrap_or(0);
    eprintln!("exit {code}");
    std::process::exit(code);
}
