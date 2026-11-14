//! An echo server (section 10.1 of ../../../docs/wasi-sockets.md): TCP or UDP ($PROTO, default tcp) on
//! $ADDR (default 0.0.0.0:7). Each answer starts with "echo ". The server
//! handles one TCP connection after the other: wasm32-wasip2 has no threads.
use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, UdpSocket};

fn main() {
    let addr = std::env::var("ADDR").unwrap_or_else(|_| "0.0.0.0:7".into());
    let proto = std::env::var("PROTO").unwrap_or_else(|_| "tcp".into());
    if proto == "udp" {
        let s = UdpSocket::bind(&addr).expect("bind");
        println!("udp echo on {}", s.local_addr().unwrap());
        let mut buf = vec![0u8; 65536];
        loop {
            let (n, from) = s.recv_from(&mut buf).expect("recv");
            println!("udp {n} bytes from {from}");
            let mut out = b"echo ".to_vec();
            out.extend_from_slice(&buf[..n]);
            let _ = s.send_to(&out, from);
        }
    }
    let l = TcpListener::bind(&addr).expect("bind");
    println!("tcp echo on {}", l.local_addr().unwrap());
    for c in l.incoming() {
        let Ok(c) = c else { continue };
        println!("tcp from {:?} to {:?}", c.peer_addr(), c.local_addr());
        // &TcpStream reads and writes: try_clone is not supported on wasip2.
        for line in BufReader::new(&c).lines() {
            let Ok(line) = line else { break };
            if (&c).write_all(format!("echo {line}\n").as_bytes()).is_err() {
                break;
            }
        }
    }
}
