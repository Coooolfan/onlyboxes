package com.coooolfan.onlyboxes.core.model

data class ExecResult(
    val exitCode: Int,
    val stdout: String,
    val stderr: String,
    val errorMessage: String?,
    val success: Boolean,
)
