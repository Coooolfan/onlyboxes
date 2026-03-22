fn main() -> Result<(), Box<dyn std::error::Error>> {
    let protoc = protoc_bin_vendored::protoc_bin_path()?;
    std::env::set_var("PROTOC", protoc);

    println!("cargo:rerun-if-changed=../../api/proto/registry/v1/registry.proto");
    println!("cargo:rerun-if-changed=build.rs");

    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .build_transport(false)
        .compile_protos(
            &["../../api/proto/registry/v1/registry.proto"],
            &["../../api/proto"],
        )?;

    Ok(())
}
