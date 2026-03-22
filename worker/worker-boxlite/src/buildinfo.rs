pub fn version() -> &'static str {
    match option_env!("ONLYBOXES_WORKER_BOXLITE_VERSION") {
        Some(version) if !version.trim().is_empty() => version,
        _ => "dev",
    }
}
