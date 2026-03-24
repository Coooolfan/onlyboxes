use tracing_subscriber::EnvFilter;

pub fn configure(level: &str, format: &str, add_source: bool) {
    let filter = EnvFilter::try_new(level).unwrap_or_else(|_| EnvFilter::new("info"));
    let builder = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_file(add_source)
        .with_line_number(add_source)
        .with_target(false);

    let _ = match format {
        "text" => builder.compact().try_init(),
        _ => builder.json().try_init(),
    };
}
