use super::CapabilityError;
use serde::{Deserialize, Serialize};

#[derive(Deserialize)]
struct EchoRequest {
    message: String,
}

#[derive(Serialize)]
struct EchoResponse {
    message: String,
}

pub async fn handle_echo(payload: &[u8]) -> Result<Vec<u8>, CapabilityError> {
    let req: EchoRequest = serde_json::from_slice(payload)
        .map_err(|e| CapabilityError::new("invalid_payload", format!("bad JSON: {e}")))?;

    tracing::debug!(message_len = req.message.len(), "echo");

    let resp = EchoResponse {
        message: req.message,
    };
    serde_json::to_vec(&resp)
        .map_err(|e| CapabilityError::new("encode_failed", format!("JSON encode failed: {e}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn test_echo_roundtrip() {
        let payload = br#"{"message":"hello"}"#;
        let result = handle_echo(payload).await.unwrap();
        let resp: serde_json::Value = serde_json::from_slice(&result).unwrap();
        assert_eq!(resp["message"], "hello");
    }

    #[tokio::test]
    async fn test_echo_empty_message() {
        let payload = br#"{"message":""}"#;
        let result = handle_echo(payload).await.unwrap();
        let resp: serde_json::Value = serde_json::from_slice(&result).unwrap();
        assert_eq!(resp["message"], "");
    }

    #[tokio::test]
    async fn test_echo_invalid_payload() {
        let err = handle_echo(b"not json").await.unwrap_err();
        assert_eq!(err.code, "invalid_payload");
    }

    #[tokio::test]
    async fn test_echo_missing_field() {
        let err = handle_echo(br#"{"foo":"bar"}"#).await.unwrap_err();
        assert_eq!(err.code, "invalid_payload");
    }
}
