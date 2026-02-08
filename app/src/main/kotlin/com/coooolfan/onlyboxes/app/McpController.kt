package com.coooolfan.onlyboxes.app

import com.coooolfan.onlyboxes.core.exception.BoxExpiredException
import com.coooolfan.onlyboxes.core.exception.BoxNotFoundException
import com.coooolfan.onlyboxes.core.exception.CodeExecutionException
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.service.CodeExecutor
import io.modelcontextprotocol.spec.McpSchema
import org.springaicommunity.mcp.annotation.McpTool
import org.springaicommunity.mcp.annotation.McpToolParam
import org.slf4j.LoggerFactory
import org.springframework.beans.factory.annotation.Value
import java.net.URLConnection
import java.nio.file.Files
import java.nio.file.Path
import java.util.Base64

private const val DEFAULT_LEASE_SECONDS = 30L

class McpController(
    private val codeExecutor: CodeExecutor,
    @param:Value("\${onlyboxes.lease.default-seconds:30}")
    private val defaultLeaseSeconds: Long = DEFAULT_LEASE_SECONDS,
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
                    name = normalizedName,
                    path = normalizedPath,
                    leaseSeconds = defaultLeaseSeconds.takeIf { it > 0L },
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
    val boxId: String,
    val output: ExecResult,
)
