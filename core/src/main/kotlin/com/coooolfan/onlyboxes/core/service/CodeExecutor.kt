package com.coooolfan.onlyboxes.core.service

import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulResult
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView

interface CodeExecutor {
    fun execute(code: String): ExecResult

    fun executeStateful(request: ExecuteStatefulRequest): ExecuteStatefulResult

    fun metrics(): RuntimeMetricsView
}
