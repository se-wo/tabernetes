// The code of the components both and bothrev. Only the sequence of the
// two exports is different.
use wasi::exports::cli::run::Guest as Run;
use wasi::exports::http::incoming_handler::Guest as Handle;
use wasi::http::types::{
    Fields, IncomingRequest, OutgoingBody, OutgoingResponse, ResponseOutparam,
};

struct Component;

impl Run for Component {
    fn run() -> Result<(), ()> {
        println!("run");
        Ok(())
    }
}

impl Handle for Component {
    fn handle(_request: IncomingRequest, out: ResponseOutparam) {
        let resp = OutgoingResponse::new(Fields::new());
        let body = resp.body().expect("body");
        ResponseOutparam::set(out, Ok(resp));
        let s = body.write().expect("stream");
        s.blocking_write_and_flush(b"handle\n").expect("write");
        drop(s);
        OutgoingBody::finish(body, None).expect("finish");
    }
}

