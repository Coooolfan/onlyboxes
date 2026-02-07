package com.coooolfan.onlyboxes.core.port

import com.coooolfan.onlyboxes.core.model.ExecResult

interface BoxSession : AutoCloseable {
    val sessionId: String

    fun run(code: String): ExecResult

    override fun close()
}
