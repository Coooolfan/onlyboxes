pub mod onlyboxes {
    pub mod registry {
        pub mod v1 {
            tonic::include_proto!("onlyboxes.registry.v1");
        }
    }
}

pub use onlyboxes::registry::v1::*;
