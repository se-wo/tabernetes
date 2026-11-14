//! A proxy component (section 10.1 of ../../../docs/wasi-sockets.md). The path of
//! the request selects the behavior:
//!
//!   /context     environment, arguments, preopens, REQUEST_ID, a counter
//!   /headers     the method and the headers of the request
//!   /status/N    an empty response with the status code N
//!   /cookie      a response with two Set-Cookie headers
//!   /wait/MS     a response after MS milliseconds
//!   /big/N       a response body of N bytes
//!   /trap        a trap
//!   /noresponse  handle returns and sets no response
//!   /error       response-outparam.set with an error code
//!   /sockets     the result of each function of wasi:sockets
//!   /fetch/URL   a client request to http://URL, the result in the body
//!
//! Paths of phase 2 (W2.3a). The paths above keep their output, because the
//! results of the Docker check depend on it.
//!
//!   /stdio                  a line to stdout and a line to stderr (PX14)
//!   /echo                   the request body as the response body, with
//!                           the method in x-method and the length of the
//!                           request body in x-body-length (PX18, PX29)
//!   /fetchfull/URL          as /fetch, and also the response headers and
//!                           the body bytes in hex (CR3 to CR5)
//!   /fetchdrop/MS/URL       a client request to http://URL; after MS
//!                           milliseconds, the component drops it without a
//!                           response and answers (CR10)
use std::sync::atomic::{AtomicU32, Ordering};

use wasi::exports::http::incoming_handler::Guest;
use wasi::http::types::{
    ErrorCode, Fields, IncomingRequest, Method, OutgoingBody, OutgoingRequest, OutgoingResponse,
    ResponseOutparam, Scheme,
};

static COUNTER: AtomicU32 = AtomicU32::new(0);

struct Component;

wasi::http::proxy::export!(Component);

fn respond(out: ResponseOutparam, status: u16, headers: &[(&str, &str)], body: &str) {
    respond_bytes(out, status, headers, body.as_bytes());
}

fn respond_bytes(out: ResponseOutparam, status: u16, headers: &[(&str, &str)], body: &[u8]) {
    let list: Vec<(String, Vec<u8>)> = headers
        .iter()
        .map(|(k, v)| (k.to_string(), v.as_bytes().to_vec()))
        .collect();
    let fields = Fields::from_list(&list).expect("headers");
    let resp = OutgoingResponse::new(fields);
    resp.set_status_code(status).expect("status");
    let ob = resp.body().expect("body");
    ResponseOutparam::set(out, Ok(resp));
    if !body.is_empty() {
        let s = ob.write().expect("stream");
        for chunk in body.chunks(4096) {
            s.blocking_write_and_flush(chunk).expect("write");
        }
        drop(s);
    }
    OutgoingBody::finish(ob, None).expect("finish");
}

fn method(m: &Method) -> String {
    match m {
        Method::Get => "GET".into(),
        Method::Head => "HEAD".into(),
        Method::Post => "POST".into(),
        Method::Put => "PUT".into(),
        Method::Delete => "DELETE".into(),
        Method::Connect => "CONNECT".into(),
        Method::Options => "OPTIONS".into(),
        Method::Trace => "TRACE".into(),
        Method::Patch => "PATCH".into(),
        Method::Other(s) => format!("other:{s}"),
    }
}

fn sleep_ms(ms: u64) {
    wasi::clocks::monotonic_clock::subscribe_duration(ms * 1_000_000).block();
}

fn sockets() -> String {
    use wasi::sockets::network::{IpAddressFamily, IpSocketAddress, Ipv4SocketAddress};
    let mut out = String::new();
    let mut log = |k: &str, v: String| out.push_str(&format!("{k}: {v}\n"));
    let net = wasi::sockets::instance_network::instance_network();
    let any = IpSocketAddress::Ipv4(Ipv4SocketAddress {
        port: 0,
        address: (0, 0, 0, 0),
    });
    let remote = IpSocketAddress::Ipv4(Ipv4SocketAddress {
        port: 80,
        address: (10, 96, 0, 1),
    });

    match wasi::sockets::tcp_create_socket::create_tcp_socket(IpAddressFamily::Ipv4) {
        Err(e) => log("tcp create", format!("{e:?}")),
        Ok(s) => {
            log("tcp create", "ok".into());
            let r = s.start_bind(&net, any);
            log("tcp start-bind", format!("{r:?}"));
            if r.is_ok() {
                s.subscribe().block();
                log("tcp finish-bind", format!("{:?}", s.finish_bind()));
                log("tcp local-address", format!("{:?}", s.local_address()));
                log("tcp start-listen", format!("{:?}", s.start_listen()));
            }
        }
    }
    match wasi::sockets::tcp_create_socket::create_tcp_socket(IpAddressFamily::Ipv4) {
        Err(e) => log("tcp2 create", format!("{e:?}")),
        Ok(s) => {
            let r = s.start_connect(&net, remote);
            log("tcp start-connect", format!("{r:?}"));
            if r.is_ok() {
                s.subscribe().block();
                log(
                    "tcp finish-connect",
                    format!("{:?}", s.finish_connect().map(|_| ())),
                );
            }
        }
    }
    match wasi::sockets::udp_create_socket::create_udp_socket(IpAddressFamily::Ipv4) {
        Err(e) => log("udp create", format!("{e:?}")),
        Ok(s) => {
            log("udp create", "ok".into());
            let r = s.start_bind(&net, any);
            log("udp start-bind", format!("{r:?}"));
            if r.is_ok() {
                s.subscribe().block();
                log("udp finish-bind", format!("{:?}", s.finish_bind()));
                log(
                    "udp stream",
                    format!("{:?}", s.stream(Some(remote)).map(|_| ())),
                );
            }
        }
    }
    match wasi::sockets::ip_name_lookup::resolve_addresses(&net, "localhost") {
        Err(e) => log("resolve", format!("{e:?}")),
        Ok(r) => {
            r.subscribe().block();
            log("resolve next", format!("{:?}", r.resolve_next_address()));
        }
    }
    out
}

/// start_fetch sends a client request to http://target.
fn start_fetch(
    target: &str,
) -> Result<wasi::http::types::FutureIncomingResponse, wasi::http::types::ErrorCode> {
    let (authority, path) = match target.split_once('/') {
        Some((a, p)) => (a.to_string(), format!("/{p}")),
        None => (target.to_string(), "/".to_string()),
    };
    let req = OutgoingRequest::new(Fields::new());
    let _ = req.set_scheme(Some(&Scheme::Http));
    let _ = req.set_authority(Some(&authority));
    let _ = req.set_path_with_query(Some(&path));
    wasi::http::outgoing_handler::handle(req, None)
}

fn read_all(body: &wasi::http::types::IncomingBody) -> Result<Vec<u8>, String> {
    let s = body.stream().map_err(|_| "no stream".to_string())?;
    let mut out = Vec::new();
    loop {
        match s.blocking_read(65536) {
            Ok(b) => out.extend_from_slice(&b),
            Err(wasi::io::streams::StreamError::Closed) => return Ok(out),
            Err(e) => return Err(format!("{e:?}")),
        }
    }
}

/// fetch_full is fetch with the response headers and the body bytes.
fn fetch_full(target: &str) -> String {
    let fut = match start_fetch(target) {
        Ok(f) => f,
        Err(e) => return format!("handle error {e:?}\n"),
    };
    fut.subscribe().block();
    let resp = match fut.get() {
        Some(Ok(Ok(resp))) => resp,
        Some(Ok(Err(e))) => return format!("error {e:?}\n"),
        other => return format!("get {:?}\n", other.map(|r| r.map(|_| ()))),
    };
    let mut out = format!("status {}\n", resp.status());
    for (k, v) in resp.headers().entries() {
        out.push_str(&format!("h {k}: {}\n", String::from_utf8_lossy(&v)));
    }
    let body = resp.consume().expect("consume");
    match read_all(&body) {
        Ok(b) => {
            let hex: String = b.iter().map(|x| format!("{x:02x}")).collect();
            out.push_str(&format!("body-len {}\nbody-hex {hex}\n", b.len()));
        }
        Err(e) => out.push_str(&format!("body error {e}\n")),
    }
    out
}

/// fetch_drop starts a client request, waits ms milliseconds and drops it.
fn fetch_drop(rest: &str) -> String {
    let (ms, target) = rest.split_once('/').unwrap_or((rest, ""));
    let ms: u64 = ms.parse().unwrap_or(1000);
    let fut = match start_fetch(target) {
        Ok(f) => f,
        Err(e) => return format!("handle error {e:?}\n"),
    };
    sleep_ms(ms);
    let state = if fut.get().is_some() {
        "ready"
    } else {
        "pending"
    };
    drop(fut);
    format!("dropped after {ms} ms, the response was {state}\n")
}

fn fetch(target: &str) -> String {
    let (authority, path) = match target.split_once('/') {
        Some((a, p)) => (a.to_string(), format!("/{p}")),
        None => (target.to_string(), "/".to_string()),
    };
    let req = OutgoingRequest::new(Fields::new());
    let _ = req.set_scheme(Some(&Scheme::Http));
    let _ = req.set_authority(Some(&authority));
    let _ = req.set_path_with_query(Some(&path));
    let start = wasi::clocks::monotonic_clock::now();
    let fut = match wasi::http::outgoing_handler::handle(req, None) {
        Ok(f) => f,
        Err(e) => return format!("handle error {e:?}\n"),
    };
    fut.subscribe().block();
    let ms = (wasi::clocks::monotonic_clock::now() - start) / 1_000_000;
    match fut.get() {
        Some(Ok(Ok(resp))) => format!("status {} after {ms} ms\n", resp.status()),
        Some(Ok(Err(e))) => format!("error {e:?} after {ms} ms\n"),
        other => format!("get {:?} after {ms} ms\n", other.map(|r| r.map(|_| ()))),
    }
}

impl Guest for Component {
    fn handle(request: IncomingRequest, out: ResponseOutparam) {
        let path = request.path_with_query().unwrap_or_default();
        let parts: Vec<&str> = path.trim_start_matches('/').splitn(2, '/').collect();
        match parts[0] {
            "context" => {
                let n = COUNTER.fetch_add(1, Ordering::Relaxed) + 1;
                let env = wasi::cli::environment::get_environment();
                let args = wasi::cli::environment::get_arguments();
                let pre = wasi::filesystem::preopens::get_directories();
                let mut body = format!("counter {n}\nargs {args:?}\npreopens {}\n", pre.len());
                for (k, v) in env {
                    body.push_str(&format!("env {k}={v}\n"));
                }
                respond(out, 200, &[("content-type", "text/plain")], &body);
            }
            "headers" => {
                let mut body = format!("method {}\n", method(&request.method()));
                for (k, v) in request.headers().entries() {
                    body.push_str(&format!("{k}: {}\n", String::from_utf8_lossy(&v)));
                }
                respond(out, 200, &[], &body);
            }
            "status" => {
                let code = parts.get(1).and_then(|s| s.parse().ok()).unwrap_or(200);
                respond(out, code, &[], "");
            }
            "cookie" => respond(
                out,
                200,
                &[("set-cookie", "a=1"), ("set-cookie", "b=2")],
                "cookies\n",
            ),
            "wait" => {
                let ms = parts.get(1).and_then(|s| s.parse().ok()).unwrap_or(1000);
                sleep_ms(ms);
                respond(out, 200, &[], &format!("waited {ms}\n"));
            }
            "big" => {
                let n: usize = parts.get(1).and_then(|s| s.parse().ok()).unwrap_or(0);
                respond(out, 200, &[], &"x".repeat(n));
            }
            "trap" => unreachable!("trap on request"),
            "noresponse" => {}
            "error" => {
                ResponseOutparam::set(out, Err(ErrorCode::InternalError(Some("test".into()))))
            }
            "sockets" => respond(out, 200, &[], &sockets()),
            "fetch" => respond(out, 200, &[], &fetch(parts.get(1).copied().unwrap_or(""))),
            "stdio" => {
                println!("stdout from the instance");
                eprintln!("stderr from the instance");
                respond(out, 200, &[], "wrote to stdout and stderr\n");
            }
            "echo" => {
                let m = method(&request.method());
                let body = request.consume().expect("consume");
                let data = read_all(&body).unwrap_or_default();
                drop(body);
                let len = data.len().to_string();
                respond_bytes(
                    out,
                    200,
                    &[("x-method", m.as_str()), ("x-body-length", len.as_str())],
                    &data,
                );
            }
            "fetchfull" => respond(
                out,
                200,
                &[],
                &fetch_full(parts.get(1).copied().unwrap_or("")),
            ),
            "fetchdrop" => respond(
                out,
                200,
                &[],
                &fetch_drop(parts.get(1).copied().unwrap_or("")),
            ),
            _ => respond(out, 404, &[], "unknown path\n"),
        }
    }
}
