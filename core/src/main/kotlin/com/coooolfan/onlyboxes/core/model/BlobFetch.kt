package com.coooolfan.onlyboxes.core.model

data class FetchBlobRequest(
    val name: String,
    val path: String,
    val leaseSeconds: Long?,
)

data class FetchedBlob(
    val path: String,
    val bytes: ByteArray,
)
