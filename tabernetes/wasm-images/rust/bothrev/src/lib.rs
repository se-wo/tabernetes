//! The component of ../../both with the export wasi:http/incoming-handler
//! first and wasi:cli/run second (CO2).
include!("../../both/src/component.rs");

wasi::http::proxy::export!(Component);
wasi::cli::command::export!(Component);
