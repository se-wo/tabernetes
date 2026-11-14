//! A command component that exits with the unstable function
//! wasi:cli/exit#exit-with-code and the code in $EXIT_CODE.
wit_bindgen::generate!({
    inline: r#"
        package wasi:cli@0.2.9;
        interface exit {
            exit: func(status: result);
            @unstable(feature = cli-exit-with-code)
            exit-with-code: func(status-code: u8);
        }
        world w { import exit; }
    "#,
    features: ["cli-exit-with-code"],
});

fn main() {
    let code: u8 = std::env::var("EXIT_CODE")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);
    println!("exit-with-code {code}");
    wasi::cli::exit::exit_with_code(code);
}
