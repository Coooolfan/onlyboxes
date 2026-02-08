package com.coooolfan.onlyboxes.app

import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.service.CodeExecutor
import org.springaicommunity.mcp.annotation.McpTool
import org.springaicommunity.mcp.annotation.McpToolParam
import org.springframework.beans.factory.annotation.Value

private const val DEFAULT_LEASE_SECONDS = 30L

class McpController(
    private val codeExecutor: CodeExecutor,
    @param:Value("\${onlyboxes.lease.default-seconds:30}")
    private val defaultLeaseSeconds: Long = DEFAULT_LEASE_SECONDS,
) {
    @McpTool(description = "Execute Python code with stateful (file-system only) container or create a new one")
    fun pythonExecuteStateful(
        @McpToolParam(
            description = "Container name, if not provided or empty, a new container will be created",
            required = false,
        )
        name: String?,
        @McpToolParam(description = "Python code to execute")
        code: String,
        @McpToolParam(
            description = "Lease seconds for this stateful container (renewal). After this time the container becomes unavailable",
            required = false,
        )
        leaseSeconds: Long?,
    ): ExecuteStatefulResponse {
        val result = codeExecutor.executeStateful(
            ExecuteStatefulRequest(
                name = name,
                code = code,
                leaseSeconds = leaseSeconds ?: defaultLeaseSeconds.takeIf { it > 0L },
            ),
        )

        return ExecuteStatefulResponse(
            boxId = result.boxId,
            output = result.output,
        )
    }

    @McpTool(description = "Execute Python code")
    fun pythonExecute(
        @McpToolParam(description = "Python code to execute")
        code: String,
    ): ExecResult {
        return codeExecutor.execute(code)
    }

    @McpTool(description = "Fetch all runtime metrics")
    fun metrics(): RuntimeMetricsView = codeExecutor.metrics()
}

data class ExecuteStatefulResponse(
    val boxId: String,
    val output: ExecResult,
)
