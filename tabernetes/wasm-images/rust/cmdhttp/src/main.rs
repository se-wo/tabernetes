//! A command component with an import of wasi:http (CR6).
fn main() {
    println!("start");
    let req = wasi::http::types::OutgoingRequest::new(wasi::http::types::Fields::new());
    let r = wasi::http::outgoing_handler::handle(req, None);
    println!("handle: {:?}", r.map(|_| ()));
}
