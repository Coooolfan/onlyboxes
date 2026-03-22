use boxlite::BoxliteRuntime;
use std::sync::Arc;

pub fn init_runtime() -> Arc<BoxliteRuntime> {
    let runtime = BoxliteRuntime::with_defaults()
        .expect("failed to initialize Boxlite runtime");
    Arc::new(runtime)
}
