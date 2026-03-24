fn main() -> Result<(), Box<dyn std::error::Error>> {
    let protoc = protoc_bin_vendored::protoc_bin_path()?;
    std::env::set_var("PROTOC", protoc);
    let out_dir = std::path::PathBuf::from(std::env::var("OUT_DIR")?);
    let test_server_out_dir = out_dir.join("test-server");
    std::fs::create_dir_all(&test_server_out_dir)?;

    println!("cargo:rerun-if-changed=../../api/proto/registry/v1/registry.proto");
    println!("cargo:rerun-if-changed=build.rs");

    tonic_build::configure()
        .build_server(false)
        .build_client(true)
        .build_transport(false)
        .compile_protos(
            &["../../api/proto/registry/v1/registry.proto"],
            &["../../api/proto"],
        )?;

    // Keep server stubs out of the runtime build, but generate a test-only copy so
    // session_client's transport tests can still stand up a fake registry server.
    tonic_build::configure()
        .out_dir(&test_server_out_dir)
        .build_client(false)
        .build_transport(false)
        .compile_protos(
            &["../../api/proto/registry/v1/registry.proto"],
            &["../../api/proto"],
        )?;

    Ok(())
}
