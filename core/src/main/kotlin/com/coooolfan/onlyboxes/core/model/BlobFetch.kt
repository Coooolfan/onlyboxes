package com.coooolfan.onlyboxes.core.model

data class FetchBlobRequest(
    val name: String,
    val path: String,
    val leaseSeconds: Long?,
)

class FetchedBlob(
    val path: String,
    bytes: ByteArray,
) {
    private val data: ByteArray = bytes.copyOf()

    val bytes: ByteArray
        get() = data.copyOf()

    override fun equals(other: Any?): Boolean {
        if (this === other) {
            return true
        }
        if (other !is FetchedBlob) {
            return false
        }
        return path == other.path && data.contentEquals(other.data)
    }

    override fun hashCode(): Int {
        return 31 * path.hashCode() + data.contentHashCode()
    }

    override fun toString(): String {
        return "FetchedBlob(path=$path, bytes=${data.size} bytes)"
    }
}
