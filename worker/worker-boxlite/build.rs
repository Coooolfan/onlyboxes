fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::configure()
        .build_server(false)
        .build_client(true)
        .build_transport(false) // avoid clash: RPC "Connect" vs tonic's connect()
        .compile_protos(
            &["../../api/proto/registry/v1/registry.proto"],
            &["../../api/proto"],
        )?;
    Ok(())
}
