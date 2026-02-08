package com.coooolfan.onlyboxes.app

import com.coooolfan.onlyboxes.core.exception.BoxExpiredException
import com.coooolfan.onlyboxes.core.exception.BoxNotFoundException
import com.coooolfan.onlyboxes.core.exception.CodeExecutionException
import com.coooolfan.onlyboxes.core.model.ActiveContainerView
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.ListActiveContainersRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.service.CodeExecutor
import io.modelcontextprotocol.spec.McpSchema
import org.springaicommunity.mcp.annotation.McpTool
import org.springaicommunity.mcp.annotation.McpToolParam
import org.slf4j.LoggerFactory
import java.net.URLConnection
import java.nio.file.Files
import java.nio.file.Path
import java.util.Base64

class McpController(
    private val codeExecutor: CodeExecutor,
    private val authTokenProvider: AuthTokenProvider,
) {
    private val logger = LoggerFactory.getLogger(McpController::class.java)

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
            description = "Lease seconds for this stateful container renewal. Non-negative values are clamped to [min,max] and start counting after command execution. Negative values destroy the container immediately after command execution.",
            required = true,
        )
        leaseSeconds: Long,
    ): ExecuteStatefulResponse {
        val result = codeExecutor.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = authTokenProvider.requireToken(),
                name = name,
                code = code,
                leaseSeconds = leaseSeconds,
            ),
        )

        return ExecuteStatefulResponse(
            boxId = result.boxId,
            destroyed = result.destroyed,
            remainingDestroySeconds = result.remainingDestroySeconds,
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

    @McpTool(description = "fetch a blob file in base64, the client will automatically decode it and display for user")
    fun fetchBlob(
        @McpToolParam(description = "Blob file path")
        path: String,
        @McpToolParam(description = "name of Container")
        name: String,
    ): McpSchema.CallToolResult {
        val normalizedPath = path.trim()
        if (normalizedPath.isEmpty()) {
            return errorCallToolResult("Blob file path must not be empty")
        }
        // TODO(boxlite-tmpfs): Remove this guard after boxlite copyOut can read runtime tmpfs mounts.
        // Current SDK resolves copyOut from shared rootfs view, so /tmp files are not fetchable.
        if (normalizedPath == "/tmp" || normalizedPath.startsWith("/tmp/")) {
            return errorCallToolResult(
                "fetchBlob does not support /tmp paths yet. " +
                    "Please move/copy the file to a non-/tmp path (for example /root/work) and retry.",
            )
        }
        val normalizedName = name.trim()
        if (normalizedName.isEmpty()) {
            return errorCallToolResult("Container name must not be empty")
        }

        val fetchedBlob = try {
            codeExecutor.fetchBlob(
                FetchBlobRequest(
                    ownerToken = authTokenProvider.requireToken(),
                    name = normalizedName,
                    path = normalizedPath,
                ),
            )
        } catch (ex: BoxNotFoundException) {
            logger.warn("fetchBlob failed: box not found. box='{}', path='{}'", normalizedName, normalizedPath, ex)
            return errorCallToolResult("Failed to fetch blob from box '$normalizedName': ${ex.message}")
        } catch (ex: BoxExpiredException) {
            logger.warn("fetchBlob failed: box expired. box='{}', path='{}'", normalizedName, normalizedPath, ex)
            return errorCallToolResult("Failed to fetch blob from box '$normalizedName': ${ex.message}")
        } catch (ex: CodeExecutionException) {
            logger.warn("fetchBlob failed: execution error. box='{}', path='{}'", normalizedName, normalizedPath, ex)
            return errorCallToolResult("Failed to fetch blob from box '$normalizedName': ${ex.message}")
        } catch (ex: IllegalArgumentException) {
            logger.warn("fetchBlob failed: invalid argument. box='{}', path='{}'", normalizedName, normalizedPath, ex)
            return errorCallToolResult("Failed to fetch blob from box '$normalizedName': ${ex.message}")
        }

        val mimeType = detectMimeType(fetchedBlob.path)
        val base64Data = Base64.getEncoder().encodeToString(fetchedBlob.bytes)

        val builder = McpSchema.CallToolResult.builder()
        return if (mimeType.startsWith("image/")) {
            builder
                .addContent(McpSchema.ImageContent(null, base64Data, mimeType))
                .build()
        } else {
            builder
                .addTextContent("mimeType=$mimeType")
                .addTextContent(base64Data)
                .build()
        }
    }

    @McpTool(description = "List active stateful containers for current user")
    fun listActiveContainers(): List<ActiveContainerView> {
        return codeExecutor.listActiveContainers(
            ListActiveContainersRequest(
                ownerToken = authTokenProvider.requireToken(),
            ),
        )
    }

    @McpTool(description = "Fetch all runtime metrics")
    fun metrics(): RuntimeMetricsView = codeExecutor.metrics()

    private fun detectMimeType(path: String): String {
        val probeMimeType = runCatching {
            Files.probeContentType(Path.of(path))
        }.getOrNull()
        if (!probeMimeType.isNullOrBlank()) {
            return probeMimeType
        }

        val guessedMimeType = URLConnection.guessContentTypeFromName(fileNameHint(path))
        return guessedMimeType ?: "application/octet-stream"
    }

    private fun fileNameHint(path: String): String {
        return runCatching {
            Path.of(path).fileName?.toString()
        }.getOrNull()?.takeIf { it.isNotBlank() } ?: path
    }

    private fun errorCallToolResult(message: String): McpSchema.CallToolResult {
        return McpSchema.CallToolResult.builder()
            .isError(true)
            .addTextContent(message)
            .build()
    }
}

data class ExecuteStatefulResponse(
    val boxId: String?,
    val destroyed: Boolean,
    val remainingDestroySeconds: Long,
    val output: ExecResult,
)
