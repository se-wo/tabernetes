//! A command component with an import of WASI 0.3 (CO11, CO12): the tab node
//! and runwasi do not give wasi:clocks@0.3.0. The component must not start,
//! and the status text must name the import.
wit_bindgen::generate!({
    inline: r#"
        package wasi:clocks@0.3.0;
        interface monotonic-clock {
            now: func() -> u64;
        }
        world w { import monotonic-clock; }
    "#,
});

fn main() {
    println!("now {}", wasi::clocks::monotonic_clock::now());
}
