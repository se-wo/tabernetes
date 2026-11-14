//! A component without wasi:cli/run and wasi:http/incoming-handler (CO3,
//! CO4). It exports the function `go`. runwasi calls it for the entrypoint
//! "/core.wasm#go"; without "#go" it looks for `_start`.
wit_bindgen::generate!({
    inline: r#"
        package poc:core;
        world w { export go: func(); }
    "#,
});

struct Component;

impl Guest for Component {
    fn go() {
        println!("go was called");
    }
}

export!(Component);
