package com.coooolfan.onlyboxes.core.service

import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulResult
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.FetchedBlob
import com.coooolfan.onlyboxes.core.model.ListActiveContainersRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.model.ActiveContainerView

interface CodeExecutor {
    fun execute(code: String): ExecResult

    fun executeStateful(request: ExecuteStatefulRequest): ExecuteStatefulResult

    fun fetchBlob(request: FetchBlobRequest): FetchedBlob

    fun listActiveContainers(request: ListActiveContainersRequest): List<ActiveContainerView>

    fun metrics(): RuntimeMetricsView
}
