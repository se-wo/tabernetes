//! A component with the export wasi:cli/run and then the export
//! wasi:http/incoming-handler (CO2). runwasi selects the world of the first
//! of the two exports. As a command, it writes "run" and ends. As a proxy, it
//! answers each request with "handle". bothrev has the other sequence.
include!("component.rs");

wasi::cli::command::export!(Component);
wasi::http::proxy::export!(Component);
