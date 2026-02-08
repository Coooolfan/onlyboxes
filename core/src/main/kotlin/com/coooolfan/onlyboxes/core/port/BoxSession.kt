package com.coooolfan.onlyboxes.core.port

import com.coooolfan.onlyboxes.core.model.ExecResult
import java.nio.file.Path

interface BoxSession : AutoCloseable {
    val sessionId: String

    fun run(code: String): ExecResult

    fun copyOut(containerSrc: String, hostDest: Path)

    override fun close()
}
