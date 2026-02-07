package com.coooolfan.onlyboxes.app

import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.service.CodeExecutor
import org.noear.solon.Solon
import org.noear.solon.ai.annotation.ToolMapping
import org.noear.solon.ai.mcp.McpChannel
import org.noear.solon.ai.mcp.server.annotation.McpServerEndpoint
import org.noear.solon.annotation.Param

@McpServerEndpoint(channel = McpChannel.STREAMABLE, mcpEndpoint = "/mcp")
class McpController(
    private val codeExecutor: CodeExecutor = RuntimeRegistry.codeExecutor,
) {
    private val defaultLeaseSeconds: Long? by lazy {
        val fromConfig = runCatching {
            Solon.cfg().getLong("onlyboxes.lease.default-seconds", 0L)
        }.getOrDefault(0L)

        val fromEnv = System.getenv("ONLYBOXES_DEFAULT_LEASE_SECONDS")
            ?.toLongOrNull()
            ?: 0L

        val resolved = if (fromConfig > 0L) fromConfig else fromEnv
        resolved.takeIf { it > 0L }
    }

    @ToolMapping(description = "Execute Python code with stateful (file-system only) container or create a new one")
    fun pythonExecuteStateful(
        @Param(description = "Container name, if not provided or empty, a new container will be created", required = false)
        name: String?,
        @Param(description = "Python code to execute")
        code: String,
        @Param(
            description = "Lease seconds for this stateful container (renewal). After this time the container becomes unavailable",
            required = false,
        )
        leaseSeconds: Long?,
    ): ExecuteStatefulResponse {
        val result = codeExecutor.executeStateful(
            ExecuteStatefulRequest(
                name = name,
                code = code,
                leaseSeconds = leaseSeconds ?: defaultLeaseSeconds,
            ),
        )

        return ExecuteStatefulResponse(
            boxId = result.boxId,
            output = result.output,
        )
    }

    @ToolMapping(description = "Execute Python code")
    fun pythonExecute(
        @Param(description = "Python code to execute")
        code: String,
    ): ExecResult {
        return codeExecutor.execute(code)
    }

    @ToolMapping(description = "Fetch all runtime metrics")
    fun metrics(): RuntimeMetricsView = codeExecutor.metrics()
}

data class ExecuteStatefulResponse(
    val boxId: String,
    val output: ExecResult,
)
