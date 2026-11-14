//! An echo server (section 10.1 of ../../../docs/wasi-sockets.md): TCP or UDP ($PROTO, default tcp) on
//! $ADDR (default 0.0.0.0:7). Each answer starts with "echo ". The server
//! handles one TCP connection after the other: wasm32-wasip2 has no threads.
//!
//! $MODE selects a test mode of phase 3 (TCP and name lookup). Each mode
//! prints one line for each fact. The step wasi-sockets of the Docker check
//! (docs/docker-node-check/wasi-sockets.sh) and the end-to-end tests compare
//! these lines.
//!
//!   (none)   the echo server above
//!   eof      a TCP server on $ADDR that reads to the end of the stream, then
//!            answers "echo <bytes>" and closes (TC13)
//!   client   connects to $CONNECT (host:port, a name is looked up), from
//!            $BIND if set; sends $LINE and a newline, closes its send
//!            direction, reads to the end and prints the answer (TC4, TC9,
//!            TC11, TC13, TC17, NL1). READ_FOREVER=1 sends nothing and
//!            only reads to the end (TC14). An error prints
//!            "error <step>: <error code>" and exits with 1.
//!   errors   the error codes of fact 12 (TC18 to TC21, rule 5 of section
//!            8.3.2, a full backlog), then a listener on [::]:$PORT6 that
//!            stays up (TC6, TC7)
//!   options  the default of each socket option, then the value after a set
//!            (TC22)
//!   poll     a TCP server on $ADDR that reads without a wait after accept
//!            and prints the milliseconds to the first byte (CO17, CO19)
//!
//! $DELAY_MS waits before the mode starts, for example until the server in
//! the other container of the pod listens.
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, TcpListener, UdpSocket};
use std::time::{Duration, Instant};

use wasi::io::streams::{InputStream, OutputStream, StreamError};
use wasi::sockets::network::{
    ErrorCode, IpAddressFamily, IpSocketAddress, Ipv4SocketAddress, Ipv6SocketAddress, Network,
};
use wasi::sockets::tcp::{ShutdownType, TcpSocket};

fn env(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.is_empty())
}

fn main() {
    if let Some(ms) = env("DELAY_MS").and_then(|v| v.parse().ok()) {
        std::thread::sleep(Duration::from_millis(ms));
    }
    let addr = env("ADDR").unwrap_or_else(|| "0.0.0.0:7".into());
    match env("MODE").unwrap_or_default().as_str() {
        "" => {}
        "eof" => serve(&addr, true),
        "client" => {
            client();
            std::process::exit(0);
        }
        "errors" => errors(),
        "options" => {
            options();
            std::process::exit(0);
        }
        "poll" => poll(&addr),
        m => {
            println!("unknown MODE {m}");
            std::process::exit(2);
        }
    }
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
    serve(&addr, false);
}

/// The TCP echo server. With `at_eof`, it reads to the end of the stream
/// first and then sends one answer.
fn serve(addr: &str, at_eof: bool) -> ! {
    let l = TcpListener::bind(addr).expect("bind");
    println!("tcp echo on {}", l.local_addr().unwrap());
    for c in l.incoming() {
        let Ok(c) = c else { continue };
        println!("tcp from {:?} to {:?}", c.peer_addr(), c.local_addr());
        if at_eof {
            let mut data = Vec::new();
            let r = (&c).read_to_end(&mut data);
            println!("tcp eof after {} bytes: {r:?}", data.len());
            let mut out = b"echo ".to_vec();
            out.extend_from_slice(&data);
            let _ = (&c).write_all(&out);
            continue;
        }
        // &TcpStream reads and writes: try_clone is not supported on wasip2.
        for line in BufReader::new(&c).lines() {
            let Ok(line) = line else { break };
            if (&c).write_all(format!("echo {line}\n").as_bytes()).is_err() {
                break;
            }
        }
    }
    unreachable!()
}

// --- wasi:sockets helpers -------------------------------------------------

fn to_std(a: IpSocketAddress) -> SocketAddr {
    match a {
        IpSocketAddress::Ipv4(a) => {
            let (x, y, z, w) = a.address;
            SocketAddr::new(Ipv4Addr::new(x, y, z, w).into(), a.port)
        }
        IpSocketAddress::Ipv6(a) => {
            let (b0, b1, b2, b3, b4, b5, b6, b7) = a.address;
            SocketAddr::new(Ipv6Addr::new(b0, b1, b2, b3, b4, b5, b6, b7).into(), a.port)
        }
    }
}

fn to_wasi(a: SocketAddr) -> IpSocketAddress {
    match a {
        SocketAddr::V4(a) => {
            let [x, y, z, w] = a.ip().octets();
            IpSocketAddress::Ipv4(Ipv4SocketAddress {
                port: a.port(),
                address: (x, y, z, w),
            })
        }
        SocketAddr::V6(a) => {
            let s = a.ip().segments();
            IpSocketAddress::Ipv6(Ipv6SocketAddress {
                port: a.port(),
                address: (s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7]),
                flow_info: 0,
                scope_id: 0,
            })
        }
    }
}

fn family(a: &SocketAddr) -> IpAddressFamily {
    if a.is_ipv4() {
        IpAddressFamily::Ipv4
    } else {
        IpAddressFamily::Ipv6
    }
}

fn sock(f: IpAddressFamily) -> Result<TcpSocket, ErrorCode> {
    wasi::sockets::tcp_create_socket::create_tcp_socket(f)
}

/// "ok" or the name of the error code.
fn res<T>(r: &Result<T, ErrorCode>) -> &'static str {
    match r {
        Ok(_) => "ok",
        Err(e) => e.name(),
    }
}

fn bind(net: &Network, s: &TcpSocket, a: SocketAddr) -> Result<(), ErrorCode> {
    s.start_bind(net, to_wasi(a))?;
    loop {
        match s.finish_bind() {
            Err(ErrorCode::WouldBlock) => s.subscribe().block(),
            r => return r,
        }
    }
}

fn listen(s: &TcpSocket) -> Result<(), ErrorCode> {
    s.start_listen()?;
    loop {
        match s.finish_listen() {
            Err(ErrorCode::WouldBlock) => s.subscribe().block(),
            r => return r,
        }
    }
}

/// connect waits for the connection, or until `max` passes: then it gives
/// None (the connection is still pending).
fn connect(
    net: &Network,
    s: &TcpSocket,
    a: SocketAddr,
    max: Option<Duration>,
) -> Option<Result<(InputStream, OutputStream), ErrorCode>> {
    if let Err(e) = s.start_connect(net, to_wasi(a)) {
        return Some(Err(e));
    }
    let end = max.map(|m| Instant::now() + m);
    loop {
        match s.finish_connect() {
            Err(ErrorCode::WouldBlock) => {}
            r => return Some(r),
        }
        let p = s.subscribe();
        match end {
            None => p.block(),
            Some(end) => {
                let left = end.saturating_duration_since(Instant::now());
                if left.is_zero() {
                    return None;
                }
                let t = wasi::clocks::monotonic_clock::subscribe_duration(left.as_nanos() as u64);
                wasi::io::poll::poll(&[&p, &t]);
            }
        }
    }
}

/// lookup gives the addresses of a name, or the address itself.
fn lookup(net: &Network, host: &str) -> Result<Vec<IpAddr>, ErrorCode> {
    if let Ok(ip) = host.parse::<IpAddr>() {
        return Ok(vec![ip]);
    }
    let st = wasi::sockets::ip_name_lookup::resolve_addresses(net, host)?;
    let mut out = Vec::new();
    loop {
        match st.resolve_next_address() {
            Ok(Some(a)) => out.push(match a {
                wasi::sockets::network::IpAddress::Ipv4((x, y, z, w)) => {
                    Ipv4Addr::new(x, y, z, w).into()
                }
                wasi::sockets::network::IpAddress::Ipv6((a, b, c, d, e, f, g, h)) => {
                    Ipv6Addr::new(a, b, c, d, e, f, g, h).into()
                }
            }),
            Ok(None) => return Ok(out),
            Err(ErrorCode::WouldBlock) => st.subscribe().block(),
            Err(e) => return Err(e),
        }
    }
}

fn fail(step: &str, e: ErrorCode) -> ! {
    println!("error {step}: {}", e.name());
    std::process::exit(1);
}

// --- client ---------------------------------------------------------------

fn client() {
    let target = env("CONNECT").expect("CONNECT=host:port");
    let (host, port) = target.rsplit_once(':').expect("CONNECT=host:port");
    let host = host.trim_start_matches('[').trim_end_matches(']');
    let port: u16 = port.parse().expect("port");
    let net = wasi::sockets::instance_network::instance_network();
    let ips = lookup(&net, host).unwrap_or_else(|e| fail("lookup", e));
    if host.parse::<IpAddr>().is_err() {
        println!("lookup {host}: {ips:?}");
    }
    let Some(ip) = ips.first() else {
        fail("lookup", ErrorCode::NameUnresolvable)
    };
    let remote = SocketAddr::new(*ip, port);
    let s = sock(family(&remote)).unwrap_or_else(|e| fail("create", e));
    if let Some(b) = env("BIND") {
        let b: SocketAddr = b.parse().expect("BIND=ip:port");
        bind(&net, &s, b).unwrap_or_else(|e| fail("bind", e));
    }
    let (rx, tx) = connect(&net, &s, remote, None)
        .unwrap()
        .unwrap_or_else(|e| fail("connect", e));
    println!(
        "local {} peer {}",
        s.local_address()
            .map(to_std)
            .map_or_else(|e| e.name().to_string(), |a| a.to_string()),
        s.remote_address()
            .map(to_std)
            .map_or_else(|e| e.name().to_string(), |a| a.to_string())
    );
    if env("READ_FOREVER").as_deref() != Some("1") {
        let line = format!("{}\n", env("LINE").unwrap_or_else(|| "hello".into()));
        if let Err(e) = tx.blocking_write_and_flush(line.as_bytes()) {
            println!("error write: {e:?}");
            std::process::exit(1);
        }
        let r = s.shutdown(ShutdownType::Send);
        println!("shutdown send: {}", res(&r));
    }
    let mut data = Vec::new();
    loop {
        match rx.blocking_read(65536) {
            Ok(b) => data.extend_from_slice(&b),
            Err(StreamError::Closed) => break,
            Err(StreamError::LastOperationFailed(e)) => {
                println!("error read: {}", e.to_debug_string());
                std::process::exit(1);
            }
        }
    }
    for l in String::from_utf8_lossy(&data).lines() {
        println!("answer {l}");
    }
    println!("eof");
    drop((rx, tx));
}

// --- errors ---------------------------------------------------------------

fn port(name: &str, def: u16) -> u16 {
    env(name).and_then(|v| v.parse().ok()).unwrap_or(def)
}

fn errors() {
    let net = wasi::sockets::instance_network::instance_network();
    let v4 = |ip: [u8; 4], p: u16| SocketAddr::from((ip, p));
    let p = port("PORT", 7100);

    // TC18 (rule 3): a second bind to the same address, first without a
    // listener, then with a listener on the first socket.
    let a = sock(IpAddressFamily::Ipv4).unwrap();
    println!(
        "tc18 bind1 0.0.0.0:{p}: {}",
        res(&bind(&net, &a, v4([0, 0, 0, 0], p)))
    );
    let b = sock(IpAddressFamily::Ipv4).unwrap();
    println!(
        "tc18 bind2 without listen: {}",
        res(&bind(&net, &b, v4([0, 0, 0, 0], p)))
    );
    println!("tc18 listen1: {}", res(&listen(&a)));
    let c = sock(IpAddressFamily::Ipv4).unwrap();
    println!(
        "tc18 bind3 with listen: {}",
        res(&bind(&net, &c, v4([0, 0, 0, 0], p)))
    );
    println!("tc18 listen2: {}", res(&listen(&b)));
    let d = sock(IpAddressFamily::Ipv4).unwrap();
    println!(
        "tc18 bind 127.0.0.1 with listen on 0.0.0.0: {}",
        res(&bind(&net, &d, v4([127, 0, 0, 1], p)))
    );
    drop((a, b, c, d));

    // TC19
    let s = sock(IpAddressFamily::Ipv4).unwrap();
    println!(
        "tc19 bind 192.0.2.7: {}",
        res(&bind(&net, &s, v4([192, 0, 2, 7], p + 1)))
    );
    drop(s);

    // TC20: an IPv6 socket and an IPv4 address, also as an IPv4-mapped
    // IPv6 address.
    let m: SocketAddr = "[::ffff:10.96.0.1]:443".parse().unwrap();
    match sock(IpAddressFamily::Ipv6) {
        Err(e) => println!("tc20 create ipv6: {}", e.name()),
        Ok(s) => {
            println!(
                "tc20 ipv6 connect 10.96.0.1:443: {}",
                res(&s.start_connect(&net, to_wasi(v4([10, 96, 0, 1], 443))))
            );
            println!(
                "tc20 ipv6 connect {m}: {}",
                res(&s.start_connect(&net, to_wasi(m)))
            );
        }
    }

    // TC21
    let s = sock(IpAddressFamily::Ipv4).unwrap();
    println!("tc21 backlog 0: {}", res(&s.set_listen_backlog_size(0)));
    drop(s);

    // Rule 5: a connect to an IPv6 address.
    let r6: SocketAddr = "[2001:db8::1]:80".parse().unwrap();
    match sock(IpAddressFamily::Ipv6) {
        Err(e) => println!("rule5 create ipv6: {}", e.name()),
        Ok(s) => {
            let r = connect(&net, &s, r6, Some(Duration::from_secs(5)));
            println!(
                "rule5 connect {r6}: {}",
                r.as_ref().map_or("pending after 5 s", res)
            );
        }
    }

    // Rule 5: a listener with a full backlog. Linux drops the SYN of a
    // client when the accept queue is full; the client sends it again
    // until the end of its SYN retries (timeout). This mode waits 2 s for
    // each connect.
    let l = sock(IpAddressFamily::Ipv4).unwrap();
    let la = v4([127, 0, 0, 1], p + 2);
    let _ = bind(&net, &l, la);
    let _ = l.set_listen_backlog_size(1);
    println!("backlog 1 listen {la}: {}", res(&listen(&l)));
    let mut keep = Vec::new();
    let mut seq = Vec::new();
    for _ in 0..4 {
        let s = sock(IpAddressFamily::Ipv4).unwrap();
        let r = connect(&net, &s, la, Some(Duration::from_secs(2)));
        seq.push(r.as_ref().map_or("pending", res));
        // The streams before the socket: a socket with streams cannot drop.
        keep.push((r, s));
    }
    println!("backlog 1 connects without accept: {}", seq.join(" "));
    drop(keep);
    drop(l);

    // TC6, TC7: an IPv6 listener on [::]. It stays up: another pod dials
    // the pod IP (IPv4) on this port.
    let p6 = port("PORT6", 7006);
    let a6: SocketAddr = SocketAddr::new(Ipv6Addr::UNSPECIFIED.into(), p6);
    let s = match sock(IpAddressFamily::Ipv6) {
        Ok(s) => s,
        Err(e) => {
            println!("tc6 create ipv6: {}", e.name());
            loop {
                std::thread::sleep(Duration::from_secs(3600));
            }
        }
    };
    let rb = bind(&net, &s, a6);
    let rl = if rb.is_ok() {
        listen(&s)
    } else {
        Err(ErrorCode::InvalidState)
    };
    println!("tc6 bind {a6}: {} listen: {}", res(&rb), res(&rl));
    loop {
        match s.accept() {
            Ok((c, rx, tx)) => {
                println!(
                    "tc6 accepted from {:?}",
                    c.remote_address().map(to_std).map_err(|e| e.name())
                );
                drop((rx, tx));
                drop(c);
            }
            Err(ErrorCode::WouldBlock) => s.subscribe().block(),
            Err(e) => {
                println!("tc6 accept: {}", e.name());
                std::thread::sleep(Duration::from_secs(3600));
            }
        }
    }
}

// --- options --------------------------------------------------------------

fn options() {
    let s = sock(IpAddressFamily::Ipv4).unwrap();
    fn show<T: std::fmt::Debug>(r: Result<T, ErrorCode>) -> String {
        match r {
            Ok(v) => format!("{v:?}"),
            Err(e) => e.name().to_string(),
        }
    }
    macro_rules! opt {
        ($name:literal, $get:ident, $set:ident, $($v:expr),+) => {{
            print!("option {} default {}", $name, show(s.$get()));
            $(
                let r = s.$set($v);
                print!(", set {:?}: {} read {}", $v, res(&r), show(s.$get()));
            )+
            println!();
        }};
    }
    opt!(
        "keep-alive-enabled",
        keep_alive_enabled,
        set_keep_alive_enabled,
        true,
        false
    );
    opt!(
        "keep-alive-idle-time",
        keep_alive_idle_time,
        set_keep_alive_idle_time,
        30_000_000_000u64,
        1_500_000_000u64,
        0u64
    );
    opt!(
        "keep-alive-interval",
        keep_alive_interval,
        set_keep_alive_interval,
        5_000_000_000u64,
        0u64
    );
    opt!(
        "keep-alive-count",
        keep_alive_count,
        set_keep_alive_count,
        3u32,
        0u32
    );
    opt!("hop-limit", hop_limit, set_hop_limit, 42u8, 0u8);
    opt!(
        "receive-buffer-size",
        receive_buffer_size,
        set_receive_buffer_size,
        65536u64,
        1u64,
        0u64
    );
    opt!(
        "send-buffer-size",
        send_buffer_size,
        set_send_buffer_size,
        65536u64,
        1u64,
        0u64
    );
}

// --- poll -----------------------------------------------------------------

fn poll(addr: &str) -> ! {
    let l = TcpListener::bind(addr).expect("bind");
    println!("tcp poll on {}", l.local_addr().unwrap());
    for c in l.incoming() {
        let Ok(c) = c else { continue };
        let t0 = Instant::now();
        println!("tcp from {:?} to {:?}", c.peer_addr(), c.local_addr());
        if let Err(e) = c.set_nonblocking(true) {
            println!("set_nonblocking: {e}");
            continue;
        }
        let mut buf = [0u8; 4096];
        let mut reads: u64 = 0;
        loop {
            reads += 1;
            match (&c).read(&mut buf) {
                Ok(n) => {
                    println!("poll {n} bytes after {} ms", t0.elapsed().as_millis());
                    break;
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => continue,
                Err(e) => {
                    println!("poll error {e} after {} ms", t0.elapsed().as_millis());
                    break;
                }
            }
        }
        eprintln!("poll reads {reads}");
    }
    unreachable!()
}
