package com.coooolfan.onlyboxes.core.service

import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulResult
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.FetchedBlob
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView

interface CodeExecutor {
    fun execute(code: String): ExecResult

    fun executeStateful(request: ExecuteStatefulRequest): ExecuteStatefulResult

    fun fetchBlob(request: FetchBlobRequest): FetchedBlob

    fun metrics(): RuntimeMetricsView
}
