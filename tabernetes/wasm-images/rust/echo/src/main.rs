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
//! The UDP modes of phase 4 use wasi:sockets/udp directly, so that they show
//! the exact error codes. The step wasi-udp of the Docker check
//! (docs/docker-node-check/wasi-udp.sh) records their lines on runwasi.
//!
//!   udp-client   sends $LINE to $CONNECT (host:port, a name is looked up;
//!                a comma-separated list sends to each target in turn) from
//!                $BIND (default 0.0.0.0:0). CONNECTED=1 gives the address
//!                to stream (a connected socket), else to send. It prints
//!                "local <addr>", "peer <addr>" when it is connected, then
//!                for each of $COUNT (default 1) datagrams "answer <text>
//!                from <addr>", "no answer" after $WAIT_MS (default 2000)
//!                or "error <step>: <error code>". It waits $INTERVAL_MS
//!                (default 1000) between the datagrams and continues after
//!                "no answer" and after an error (UD2, UD7 to UD13, UD15,
//!                UD20, UD21, FL)
//!   udp-errors   the facts of rules 2, 4, 5, 6 and 7 of section 8.3.3
//!                with the fixed ports 7200 to 7299 (UD3, UD4, UD8, UD9,
//!                UD14), and the addresses of fact 10. $POD_IP is the pod
//!                IP; without it the mode finds the source address of its
//!                default route.
//!   udp-options  the default of each UDP option, then the value after a
//!                set (the UDP part of TC22)
//!   udp-queue    a UDP server on $ADDR that waits $READ_DELAY_MS (default
//!                3000) before its first receive, then receives until 1
//!                second without a datagram and prints "received <n>
//!                datagrams of <size> bytes" (UD5, UD6)
//!   udp-flood    sends $COUNT (default 1000) datagrams of $SIZE (default
//!                1024) bytes to $CONNECT and prints "sent <n> errors <e>"
//!                (UD5, UD6)
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
        "udp-client" => std::process::exit(udp_client()),
        "udp-errors" => {
            udp_errors();
            std::process::exit(0);
        }
        "udp-options" => {
            udp_options();
            std::process::exit(0);
        }
        "udp-queue" => {
            udp_queue(&addr);
            std::process::exit(0);
        }
        "udp-flood" => {
            udp_flood();
            std::process::exit(0);
        }
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

// --- UDP (phase 4) --------------------------------------------------------

use wasi::sockets::udp::{
    IncomingDatagram, IncomingDatagramStream, OutgoingDatagram, OutgoingDatagramStream,
};

/// A UDP socket with its streams. The fields drop in this sequence: the
/// streams before the socket, as wasi:sockets requires.
struct Udp {
    rx: Option<IncomingDatagramStream>,
    tx: Option<OutgoingDatagramStream>,
    s: wasi::sockets::udp::UdpSocket,
}

/// The first port of the ephemeral range of Linux, and the last.
const EPHEMERAL: std::ops::RangeInclusive<u16> = 32768..=60999;

impl Udp {
    fn new(f: IpAddressFamily) -> Result<Udp, ErrorCode> {
        Ok(Udp {
            rx: None,
            tx: None,
            s: wasi::sockets::udp_create_socket::create_udp_socket(f)?,
        })
    }

    fn bind(&self, net: &Network, a: SocketAddr) -> Result<(), ErrorCode> {
        self.s.start_bind(net, to_wasi(a))?;
        loop {
            match self.s.finish_bind() {
                Err(ErrorCode::WouldBlock) => self.s.subscribe().block(),
                r => return r,
            }
        }
    }

    /// stream drops the old streams first: a call of stream while they are
    /// alive can trap.
    fn stream(&mut self, remote: Option<SocketAddr>) -> Result<(), ErrorCode> {
        self.rx = None;
        self.tx = None;
        let (rx, tx) = self.s.stream(remote.map(to_wasi))?;
        self.rx = Some(rx);
        self.tx = Some(tx);
        Ok(())
    }

    fn local(&self) -> String {
        addr_text(self.s.local_address())
    }

    /// check_send waits for a permit, at most 2 seconds.
    fn check_send(&self) -> Result<u64, ErrorCode> {
        let tx = self.tx.as_ref().ok_or(ErrorCode::InvalidState)?;
        let end = Instant::now() + Duration::from_secs(2);
        loop {
            match tx.check_send()? {
                0 => {}
                n => return Ok(n),
            }
            let left = end.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return Ok(0);
            }
            let t = wasi::clocks::monotonic_clock::subscribe_duration(left.as_nanos() as u64);
            wasi::io::poll::poll(&[&tx.subscribe(), &t]);
        }
    }

    /// send gives "ok", or the step and the error code: "check-send" when
    /// check-send failed, else "send". wasmtime sends nothing and gives 0
    /// while its socket is not writable yet (also for the first send after
    /// stream): then send waits for check-send again, at most 3 times.
    fn send(&self, data: &[u8], to: Option<SocketAddr>) -> Result<u64, (&'static str, ErrorCode)> {
        for _ in 0..3 {
            match self.check_send() {
                Ok(0) => return Err(("check-send", ErrorCode::WouldBlock)),
                Ok(_) => {}
                Err(e) => return Err(("check-send", e)),
            }
            let d = OutgoingDatagram {
                data: data.to_vec(),
                remote_address: to.map(to_wasi),
            };
            match self.tx.as_ref().unwrap().send(&[d]) {
                Ok(0) => continue,
                r => return r.map_err(|e| ("send", e)),
            }
        }
        Ok(0)
    }

    /// receive gives the datagrams of one receive call, after a wait of at
    /// most `wait` for the first one.
    fn receive(&self, max: u64, wait: Duration) -> Result<Vec<IncomingDatagram>, ErrorCode> {
        let rx = self.rx.as_ref().ok_or(ErrorCode::InvalidState)?;
        let end = Instant::now() + wait;
        loop {
            let v = rx.receive(max)?;
            let left = end.saturating_duration_since(Instant::now());
            if !v.is_empty() || left.is_zero() {
                return Ok(v);
            }
            let t = wasi::clocks::monotonic_clock::subscribe_duration(left.as_nanos() as u64);
            wasi::io::poll::poll(&[&rx.subscribe(), &t]);
        }
    }
}

fn addr_text(r: Result<IpSocketAddress, ErrorCode>) -> String {
    r.map(to_std)
        .map_or_else(|e| e.name().to_string(), |a| a.to_string())
}

/// The address with the port "ephemeral" when it is in 32768 to 60999.
fn eph(a: SocketAddr) -> String {
    if EPHEMERAL.contains(&a.port()) {
        match a {
            SocketAddr::V4(v) => format!("{}:ephemeral", v.ip()),
            SocketAddr::V6(v) => format!("[{}]:ephemeral", v.ip()),
        }
    } else {
        a.to_string()
    }
}

fn eph_text(r: Result<IpSocketAddress, ErrorCode>) -> String {
    r.map(to_std).map_or_else(|e| e.name().to_string(), eph)
}

/// The number, or the name of the error code.
fn nres(r: &Result<u64, ErrorCode>) -> String {
    match r {
        Ok(n) => n.to_string(),
        Err(e) => e.name().into(),
    }
}

fn sres(r: &Result<u64, (&'static str, ErrorCode)>) -> String {
    match r {
        Ok(1) => "ok".into(),
        Ok(n) => format!("ok {n}"),
        Err(("send", e)) => e.name().into(),
        Err((step, e)) => format!("{step} {}", e.name()),
    }
}

fn rres(r: &Result<Vec<IncomingDatagram>, ErrorCode>) -> String {
    match r {
        Ok(v) if v.is_empty() => "empty".into(),
        Ok(v) => {
            let sizes: Vec<String> = v.iter().map(|d| d.data.len().to_string()).collect();
            format!("{} datagrams of {} bytes", v.len(), sizes.join(" "))
        }
        Err(e) => e.name().into(),
    }
}

fn ms(name: &str, def: u64) -> Duration {
    Duration::from_millis(env(name).and_then(|v| v.parse().ok()).unwrap_or(def))
}

fn num(name: &str, def: u64) -> u64 {
    env(name).and_then(|v| v.parse().ok()).unwrap_or(def)
}

/// target looks up host:port and gives the first address.
fn target(net: &Network, t: &str) -> Result<SocketAddr, ErrorCode> {
    let (host, port) = t.rsplit_once(':').expect("host:port");
    let host = host.trim_start_matches('[').trim_end_matches(']');
    let port: u16 = port.parse().expect("port");
    let ips = lookup(net, host)?;
    if host.parse::<IpAddr>().is_err() {
        println!("lookup {host}: {ips:?}");
    }
    let ip = ips.first().ok_or(ErrorCode::NameUnresolvable)?;
    Ok(SocketAddr::new(*ip, port))
}

// --- udp-client -----------------------------------------------------------

fn udp_client() -> i32 {
    let net = wasi::sockets::instance_network::instance_network();
    let spec = env("CONNECT").expect("CONNECT=host:port");
    let mut targets = Vec::new();
    for t in spec.split(',') {
        match target(&net, t.trim()) {
            Ok(a) => targets.push(a),
            Err(e) => {
                println!("error lookup: {}", e.name());
                return 1;
            }
        }
    }
    let connected = env("CONNECTED").as_deref() == Some("1");
    let bind: SocketAddr = env("BIND")
        .unwrap_or_else(|| "0.0.0.0:0".into())
        .parse()
        .expect("BIND=ip:port");
    let mut u = match Udp::new(family(&bind)) {
        Ok(u) => u,
        Err(e) => {
            println!("error create: {}", e.name());
            return 1;
        }
    };
    if let Err(e) = u.bind(&net, bind) {
        println!("error bind: {}", e.name());
        return 1;
    }
    if let Err(e) = u.stream(connected.then(|| targets[0])) {
        println!("error stream: {}", e.name());
        return 1;
    }
    println!("local {}", u.local());
    if connected {
        println!("peer {}", addr_text(u.s.remote_address()));
    }
    let line = env("LINE").unwrap_or_else(|| "hello".into());
    let count = num("COUNT", 1);
    let wait = ms("WAIT_MS", 2000);
    for i in 0..count {
        if i > 0 {
            std::thread::sleep(ms("INTERVAL_MS", 1000));
        }
        let mut sent = 0;
        for (k, t) in targets.iter().enumerate() {
            let text = if targets.len() > 1 {
                format!("{line}-{}", k + 1)
            } else {
                line.clone()
            };
            let to = if connected { None } else { Some(*t) };
            match u.send(text.as_bytes(), to) {
                Ok(_) => sent += 1,
                Err((step, e)) => println!("error {step}: {}", e.name()),
            }
        }
        if sent == 0 {
            continue;
        }
        // Wait for one answer for each target that got a datagram.
        let end = Instant::now() + wait;
        let mut got = 0;
        loop {
            let left = end.saturating_duration_since(Instant::now());
            match u.receive(16, left) {
                Ok(v) if v.is_empty() => {
                    if got == 0 {
                        println!("no answer");
                    }
                    break;
                }
                Ok(v) => {
                    for d in v {
                        let text = String::from_utf8_lossy(&d.data);
                        println!(
                            "answer {} from {}",
                            text.trim_end_matches('\n'),
                            to_std(d.remote_address)
                        );
                        got += 1;
                    }
                    if got >= sent {
                        break;
                    }
                }
                Err(e) => {
                    println!("error receive: {}", e.name());
                    break;
                }
            }
        }
    }
    0
}

// --- udp-errors -----------------------------------------------------------

/// pod_ip gives $POD_IP, or the source address of the default route: a
/// connected UDP socket sends nothing.
fn pod_ip(net: &Network) -> Ipv4Addr {
    if let Some(ip) = env("POD_IP").and_then(|v| v.parse().ok()) {
        return ip;
    }
    let mut u = Udp::new(IpAddressFamily::Ipv4).unwrap();
    let _ = u.bind(net, SocketAddr::from(([0, 0, 0, 0], 0)));
    let _ = u.stream(Some(SocketAddr::from(([192, 0, 2, 1], 9))));
    match u.s.local_address().map(to_std) {
        Ok(SocketAddr::V4(a)) => *a.ip(),
        _ => Ipv4Addr::LOCALHOST,
    }
}

fn udp_errors() {
    let net = wasi::sockets::instance_network::instance_network();
    let v4 = |ip: [u8; 4], p: u16| SocketAddr::from((ip, p));
    let any = |p: u16| v4([0, 0, 0, 0], p);
    let lo = |p: u16| v4([127, 0, 0, 1], p);
    let pip = pod_ip(&net);
    let pod = |p: u16| SocketAddr::new(pip.into(), p);
    let new4 = || Udp::new(IpAddressFamily::Ipv4).unwrap();
    let short = Duration::from_millis(300);

    // Fact 1, rule 2: the ports of UDP.
    let a = new4();
    println!("rule2 bind 0.0.0.0:7200: {}", res(&a.bind(&net, any(7200))));
    let b = new4();
    println!(
        "rule2 bind 0.0.0.0:7200 again: {}",
        res(&b.bind(&net, any(7200)))
    );
    let c = new4();
    println!(
        "rule2 bind 127.0.0.1:7200 with 0.0.0.0:7200: {}",
        res(&c.bind(&net, lo(7200)))
    );
    drop((a, b, c));
    let a = new4();
    let b = new4();
    println!(
        "rule2 bind 127.0.0.1:7201: {} then 0.0.0.0:7201: {}",
        res(&a.bind(&net, lo(7201))),
        res(&b.bind(&net, any(7201)))
    );
    drop((a, b));
    let a = new4();
    let b = new4();
    println!(
        "rule2 udp bind pod-ip:7202: {} then 127.0.0.1:7202: {}",
        res(&a.bind(&net, pod(7202))),
        res(&b.bind(&net, lo(7202)))
    );
    drop((a, b));
    let a = new4();
    let b = new4();
    println!(
        "rule2 udp bind 127.0.0.1:7203: {} then pod-ip:7203: {}",
        res(&a.bind(&net, lo(7203))),
        res(&b.bind(&net, pod(7203)))
    );
    drop((a, b));
    let t = sock(IpAddressFamily::Ipv4).unwrap();
    let rb = bind(&net, &t, pod(7204));
    let rl = listen(&t);
    let t2 = sock(IpAddressFamily::Ipv4).unwrap();
    let rb2 = bind(&net, &t2, lo(7204));
    let rl2 = listen(&t2);
    println!(
        "rule2 tcp listen pod-ip:7204: {} {} then 127.0.0.1:7204: {} {}",
        res(&rb),
        res(&rl),
        res(&rb2),
        res(&rl2)
    );
    let u = new4();
    println!(
        "rule2 udp bind 0.0.0.0:7204 with tcp listeners on 7204: {}",
        res(&u.bind(&net, any(7204)))
    );
    drop((u, t, t2));
    let mut u = new4();
    println!("rule2 local before bind: {}", u.local());
    println!("rule4 stream without bind: {}", res(&u.stream(None)));
    let r = u.bind(&net, any(0));
    println!(
        "rule2 bind 0.0.0.0:0: {} local {}",
        res(&r),
        eph_text(u.s.local_address())
    );
    println!("fact port of bind 0.0.0.0:0: {}", u.local());
    let w = new4();
    let r = w.bind(&net, lo(0));
    println!(
        "rule3 bind 127.0.0.1:0: {} local {}",
        res(&r),
        eph_text(w.s.local_address())
    );
    drop((u, w));

    // Fact 2, rule 5: the largest datagram.
    let mut srv = new4();
    let _ = srv.bind(&net, lo(7210));
    let _ = srv.stream(None);
    let mut cl = new4();
    let _ = cl.bind(&net, lo(0));
    println!(
        "rule5 stream 127.0.0.1:7210: {}",
        res(&cl.stream(Some(lo(7210))))
    );
    println!(
        "rule5 send 65507: {}",
        sres(&cl.send(&vec![b'x'; 65507], None))
    );
    println!(
        "rule5 send 65508: {}",
        sres(&cl.send(&vec![b'x'; 65508], None))
    );
    println!(
        "rule5 check-send after 65508: {}",
        nres(&cl.tx.as_ref().unwrap().check_send())
    );
    println!(
        "rule5 send 5 after 65508: {}",
        sres(&cl.send(b"after", None))
    );
    let mut un = new4();
    let _ = un.bind(&net, any(0));
    let _ = un.stream(None);
    println!(
        "rule5 unconnected send 65508: {}",
        sres(&un.send(&vec![b'x'; 65508], Some(lo(7210))))
    );
    let mut sizes = Vec::new();
    while let Ok(v) = srv.receive(16, short) {
        if v.is_empty() {
            break;
        }
        sizes.extend(v.iter().map(|d| d.data.len().to_string()));
    }
    println!("rule5 server received: {}", sizes.join(" "));
    drop((cl, un));

    // Fact 6, rule 6: check-send and the size of one receive.
    let mut un = new4();
    let _ = un.bind(&net, any(0));
    let _ = un.stream(None);
    println!(
        "rule6 check-send unconnected: {}",
        nres(&un.tx.as_ref().unwrap().check_send())
    );
    let mut cl = new4();
    let _ = cl.bind(&net, any(0));
    let _ = cl.stream(Some(lo(7210)));
    println!(
        "rule6 check-send connected: {}",
        nres(&cl.tx.as_ref().unwrap().check_send())
    );
    let _ = cl.send(b"1", None);
    let mut fresh = new4();
    let _ = fresh.bind(&net, any(0));
    let _ = fresh.stream(Some(lo(7299)));
    let tx = fresh.tx.as_ref().unwrap();
    let c = nres(&tx.check_send());
    let r = tx.send(&[OutgoingDatagram {
        data: b"1".to_vec(),
        remote_address: None,
    }]);
    println!(
        "rule6 at once after stream: check-send {c}, send {}, check-send {}",
        nres(&r),
        nres(&tx.check_send())
    );
    drop(fresh);
    println!(
        "rule6 check-send after a send: {}",
        nres(&cl.tx.as_ref().unwrap().check_send())
    );
    for _ in 0..39 {
        let _ = cl.send(b"1", None);
    }
    std::thread::sleep(short);
    println!(
        "rule6 receive 100 after 40 datagrams: {}",
        srv.receive(100, short)
            .map_or_else(|e| e.name().to_string(), |v| v.len().to_string())
    );
    println!(
        "rule6 receive 0: {}",
        srv.receive(0, Duration::ZERO)
            .map_or_else(|e| e.name().to_string(), |v| v.len().to_string())
    );
    println!(
        "rule6 receive max: {}",
        srv.receive(u64::MAX, short)
            .map_or_else(|e| e.name().to_string(), |v| v.len().to_string())
    );
    while let Ok(v) = srv.receive(100, short) {
        if v.is_empty() {
            break;
        }
    }
    drop((un, cl));

    // Fact 3, rule 7: a connected socket to a port without a socket.
    let mut cl = new4();
    let _ = cl.bind(&net, any(0));
    let _ = cl.stream(Some(lo(7220)));
    let mut seq = vec![sres(&cl.send(b"1", None))];
    std::thread::sleep(short);
    seq.push(rres(&cl.receive(16, Duration::ZERO)));
    seq.push(rres(&cl.receive(16, Duration::ZERO)));
    seq.push(sres(&cl.send(b"2", None)));
    std::thread::sleep(short);
    seq.push(sres(&cl.send(b"3", None)));
    seq.push(sres(&cl.send(b"4", None)));
    std::thread::sleep(short);
    seq.push(rres(&cl.receive(16, Duration::ZERO)));
    println!(
        "rule7 connected send, receive, receive, send, send, send, receive: {}",
        seq.join(", ")
    );
    println!(
        "rule7 check-send with a pending error: {}",
        nres(&cl.tx.as_ref().unwrap().check_send())
    );
    drop(cl);
    // A receive that waits on its pollable.
    let mut cl = new4();
    let _ = cl.bind(&net, any(0));
    let _ = cl.stream(Some(lo(7225)));
    let s1 = sres(&cl.send(b"1", None));
    let t0 = Instant::now();
    let r = rres(&cl.receive(16, Duration::from_secs(1)));
    println!(
        "rule7 connected send {s1}, receive with a wait of 1 s: {r} after {} ms, send: {}",
        t0.elapsed().as_millis() / 100 * 100,
        sres(&cl.send(b"2", None))
    );
    drop(cl);
    // Two refused datagrams before a call: how many errors?
    let mut cl = new4();
    let _ = cl.bind(&net, any(0));
    let _ = cl.stream(Some(lo(7221)));
    let mut seq = vec![sres(&cl.send(b"1", None))];
    std::thread::sleep(short);
    seq.push(sres(&cl.send(b"2", None)));
    std::thread::sleep(short);
    for _ in 0..3 {
        seq.push(rres(&cl.receive(16, Duration::ZERO)));
    }
    println!(
        "rule7 connected send, send, receive, receive, receive: {}",
        seq.join(", ")
    );
    drop(cl);
    // Does the send that takes the error send its datagram?
    let mut cl = new4();
    let _ = cl.bind(&net, any(0));
    let _ = cl.stream(Some(lo(7222)));
    let s1 = sres(&cl.send(b"1", None));
    std::thread::sleep(short);
    let mut late = new4();
    let _ = late.bind(&net, lo(7222));
    let _ = late.stream(None);
    let s2 = sres(&cl.send(b"2", None));
    let s3 = sres(&cl.send(b"3", None));
    let mut got = Vec::new();
    while let Ok(v) = late.receive(16, short) {
        if v.is_empty() {
            break;
        }
        got.extend(
            v.iter()
                .map(|d| String::from_utf8_lossy(&d.data).to_string()),
        );
    }
    println!(
        "rule7 send 1: {s1}, a server binds, send 2: {s2}, send 3: {s3}, the server receives: {}",
        got.join(" ")
    );
    drop((cl, late));
    // A socket that is not connected.
    let mut un = new4();
    let _ = un.bind(&net, any(0));
    let _ = un.stream(None);
    let mut seq = vec![sres(&un.send(b"1", Some(lo(7223))))];
    std::thread::sleep(short);
    seq.push(rres(&un.receive(16, Duration::ZERO)));
    seq.push(sres(&un.send(b"2", Some(lo(7223)))));
    std::thread::sleep(short);
    seq.push(sres(&un.send(b"3", Some(lo(7223)))));
    seq.push(rres(&un.receive(16, Duration::ZERO)));
    println!(
        "rule7 unconnected send, receive, send, send, receive: {}",
        seq.join(", ")
    );
    drop(un);
    // The pod IP: 127.0.0.1:7224 has a socket, the pod IP with the port
    // 7224 has none (UD21 in the same pod).
    let mut s = new4();
    let _ = s.bind(&net, lo(7224));
    let _ = s.stream(None);
    let mut cl = new4();
    let _ = cl.bind(&net, any(0));
    let _ = cl.stream(Some(pod(7224)));
    let s1 = sres(&cl.send(b"1", None));
    std::thread::sleep(short);
    let s2 = sres(&cl.send(b"2", None));
    println!(
        "rule7 connected to pod-ip:7224 with a socket on 127.0.0.1:7224: send {s1}, send {s2}"
    );
    drop((s, cl));

    // Fact 5, rule 4: the examination of the addresses.
    let mut un = new4();
    let _ = un.bind(&net, any(0));
    let _ = un.stream(None);
    println!(
        "rule4 send without address, not connected: {}",
        sres(&un.send(b"x", None))
    );
    for (name, a) in [
        ("255.255.255.255:7", v4([255, 255, 255, 255], 7)),
        ("224.0.0.1:7", v4([224, 0, 0, 1], 7)),
        ("0.0.0.0:7", any(7)),
        ("127.0.0.1:0", lo(0)),
        ("[2001:db8::1]:7", "[2001:db8::1]:7".parse().unwrap()),
        (
            "[::ffff:127.0.0.1]:7",
            "[::ffff:127.0.0.1]:7".parse().unwrap(),
        ),
    ] {
        println!("rule4 send to {name}: {}", sres(&un.send(b"x", Some(a))));
        let mut c = new4();
        let _ = c.bind(&net, any(0));
        println!("rule4 stream {name}: {}", res(&c.stream(Some(a))));
    }
    let mut cl = new4();
    let _ = cl.bind(&net, any(0));
    let _ = cl.stream(Some(lo(7210)));
    println!(
        "rule4 connected send to another address: {}",
        sres(&cl.send(b"x", Some(lo(7211))))
    );
    println!(
        "rule4 connected send to its remote address: {}",
        sres(&cl.send(b"x", Some(lo(7210))))
    );
    println!(
        "rule4 stream without address on a connected socket: {} remote {}",
        res(&cl.stream(None)),
        addr_text(cl.s.remote_address())
    );
    drop((un, cl));
    let mut v6 = match Udp::new(IpAddressFamily::Ipv6) {
        Ok(u) => u,
        Err(e) => {
            println!("rule4 create ipv6: {}", e.name());
            udp_addresses(&net);
            return;
        }
    };
    let any6: SocketAddr = "[::]:0".parse().unwrap();
    println!("rule4 ipv6 bind [::]:0: {}", res(&v6.bind(&net, any6)));
    println!(
        "rule4 ipv6 stream [2001:db8::1]:7: {}",
        res(&v6.stream(Some("[2001:db8::1]:7".parse().unwrap())))
    );
    let mut u6 = Udp::new(IpAddressFamily::Ipv6).unwrap();
    let _ = u6.bind(&net, any6);
    let _ = u6.stream(None);
    println!(
        "rule4 ipv6 send to [2001:db8::1]:7: {}",
        sres(&u6.send(b"x", Some("[2001:db8::1]:7".parse().unwrap())))
    );
    println!(
        "rule4 ipv6 send to 127.0.0.1:7: {}",
        sres(&u6.send(b"x", Some(lo(7))))
    );
    println!(
        "rule4 ipv6 send to [::ffff:127.0.0.1]:7: {}",
        sres(&u6.send(b"x", Some("[::ffff:127.0.0.1]:7".parse().unwrap())))
    );
    println!(
        "rule4 ipv6 send to [ff02::1]:7: {}",
        sres(&u6.send(b"x", Some("[ff02::1]:7".parse().unwrap())))
    );
    drop((v6, u6));
    // An IPv6 socket on ::1 sends to ::1 of its own pod.
    let mut s6 = Udp::new(IpAddressFamily::Ipv6).unwrap();
    let rb = s6.bind(&net, "[::1]:7230".parse().unwrap());
    let _ = s6.stream(None);
    let mut c6 = Udp::new(IpAddressFamily::Ipv6).unwrap();
    let rc = c6.bind(&net, "[::1]:0".parse().unwrap());
    let _ = c6.stream(None);
    let rs = sres(&c6.send(b"v6", Some("[::1]:7230".parse().unwrap())));
    let rr = s6.receive(16, short);
    let from = match &rr {
        Ok(v) if !v.is_empty() => format!(" from {}", eph(to_std(v[0].remote_address))),
        _ => String::new(),
    };
    println!(
        "rule4 ipv6 bind [::1]:7230: {}, bind [::1]:0: {}, send to [::1]:7230: {rs}, receive: {}{from}",
        res(&rb),
        res(&rc),
        rres(&rr)
    );
    let mut d4 = new4();
    let _ = d4.bind(&net, any(7231));
    let _ = d4.stream(None);
    let s6 = Udp::new(IpAddressFamily::Ipv6).unwrap();
    let rb6 = s6.bind(&net, "[::]:7231".parse().unwrap());
    println!("rule2 ipv6 bind [::]:7231 with 0.0.0.0:7231: {}", res(&rb6));
    drop((s6, c6, d4));
    udp_addresses(&net);
}

/// Fact 10: the addresses in the same pod.
fn udp_addresses(net: &Network) {
    let v4 = |ip: [u8; 4], p: u16| SocketAddr::from((ip, p));
    let short = Duration::from_millis(300);
    let mut srv = Udp::new(IpAddressFamily::Ipv4).unwrap();
    let _ = srv.bind(net, v4([0, 0, 0, 0], 7240));
    let _ = srv.stream(None);
    let mut cl = Udp::new(IpAddressFamily::Ipv4).unwrap();
    let _ = cl.bind(net, v4([0, 0, 0, 0], 0));
    let _ = cl.stream(None);
    let rs = sres(&cl.send(b"q", Some(v4([127, 0, 0, 1], 7240))));
    println!(
        "addr local of 0.0.0.0:0 after a send: {}",
        eph_text(cl.s.local_address())
    );
    let r = srv.receive(16, short);
    let mut line = format!("addr 0.0.0.0:0 sends to 127.0.0.1:7240 (server on 0.0.0.0:7240): {rs}");
    if let Ok(v) = &r {
        if let Some(d) = v.first() {
            let from = to_std(d.remote_address);
            line += &format!(", the server sees {}", eph(from));
            let _ = srv.send(b"a", Some(from));
            match cl.receive(16, short) {
                Ok(v) if !v.is_empty() => {
                    line += &format!(", the answer comes from {}", to_std(v[0].remote_address))
                }
                other => line += &format!(", the answer: {}", rres(&other)),
            }
        }
    } else {
        line += &format!(", the server: {}", rres(&r));
    }
    println!("{line}");
}

// --- udp-options ----------------------------------------------------------

fn udp_options() {
    let u = Udp::new(IpAddressFamily::Ipv4).unwrap();
    let s = &u.s;
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
        "unicast-hop-limit",
        unicast_hop_limit,
        set_unicast_hop_limit,
        42u8,
        255u8,
        0u8
    );
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
    // After a bind: the same values?
    let net = wasi::sockets::instance_network::instance_network();
    let b = Udp::new(IpAddressFamily::Ipv4).unwrap();
    let _ = b.bind(&net, SocketAddr::from(([0, 0, 0, 0], 0)));
    println!(
        "option after bind unicast-hop-limit {} receive-buffer-size {} send-buffer-size {}",
        show(b.s.unicast_hop_limit()),
        show(b.s.receive_buffer_size()),
        show(b.s.send_buffer_size())
    );
    match Udp::new(IpAddressFamily::Ipv6) {
        Ok(v6) => println!(
            "option ipv6 unicast-hop-limit {} receive-buffer-size {} send-buffer-size {}",
            show(v6.s.unicast_hop_limit()),
            show(v6.s.receive_buffer_size()),
            show(v6.s.send_buffer_size())
        ),
        Err(e) => println!("option ipv6 create: {}", e.name()),
    }
}

// --- udp-queue, udp-flood -------------------------------------------------

fn udp_queue(addr: &str) {
    let net = wasi::sockets::instance_network::instance_network();
    let a: SocketAddr = addr.parse().expect("ADDR=ip:port");
    let mut u = Udp::new(family(&a)).unwrap();
    if let Err(e) = u.bind(&net, a) {
        println!("error bind: {}", e.name());
        return;
    }
    let _ = u.stream(None);
    println!(
        "udp queue on {} receive-buffer-size {}",
        u.local(),
        u.s.receive_buffer_size()
            .map_or_else(|e| e.name().to_string(), |v| v.to_string())
    );
    std::thread::sleep(ms("READ_DELAY_MS", 3000));
    let (mut n, mut total, mut size) = (0u64, 0u64, 0usize);
    loop {
        match u.receive(64, Duration::from_secs(1)) {
            Ok(v) if v.is_empty() => break,
            Ok(v) => {
                for d in v {
                    n += 1;
                    total += d.data.len() as u64;
                    size = size.max(d.data.len());
                }
            }
            Err(e) => {
                println!("error receive: {}", e.name());
                break;
            }
        }
    }
    println!("received {n} datagrams of {size} bytes");
    println!("received bytes {total}");
}

fn udp_flood() {
    let net = wasi::sockets::instance_network::instance_network();
    let t = match target(&net, &env("CONNECT").expect("CONNECT=host:port")) {
        Ok(t) => t,
        Err(e) => {
            println!("error lookup: {}", e.name());
            return;
        }
    };
    let mut u = Udp::new(family(&t)).unwrap();
    let any = if t.is_ipv4() { "0.0.0.0:0" } else { "[::]:0" };
    let _ = u.bind(&net, any.parse().unwrap());
    if let Err(e) = u.stream(Some(t)) {
        println!("error stream: {}", e.name());
        return;
    }
    let data = vec![b'f'; num("SIZE", 1024) as usize];
    let (mut n, mut errs) = (0, 0);
    let mut codes = std::collections::BTreeMap::new();
    for _ in 0..num("COUNT", 1000) {
        match u.send(&data, None) {
            Ok(_) => n += 1,
            Err((step, e)) => {
                errs += 1;
                *codes.entry(format!("{step} {}", e.name())).or_insert(0) += 1;
            }
        }
    }
    println!("sent {n} errors {errs}");
    if !codes.is_empty() {
        println!("errors {codes:?}");
    }
}
