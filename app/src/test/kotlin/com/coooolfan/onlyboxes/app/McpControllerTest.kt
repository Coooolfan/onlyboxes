package com.coooolfan.onlyboxes.app

import com.coooolfan.onlyboxes.core.exception.CodeExecutionException
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulResult
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.FetchedBlob
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.service.CodeExecutor
import io.modelcontextprotocol.spec.McpSchema
import java.util.Base64
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue

class McpControllerTest {
    @Test
    fun statefulExecutionDelegatesToCodeExecutor() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor)

        val result = controller.pythonExecuteStateful(
            name = "box-1",
            code = "print('hello')",
            leaseSeconds = 20,
        )

        assertEquals("box-1", result.boxId)
        assertEquals("print('hello')", executor.lastStatefulRequest?.code)
        assertEquals("out:print('hello')", result.output.stdout)
    }

    @Test
    fun statelessExecutionDelegatesToCodeExecutor() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor)

        val result = controller.pythonExecute("print('x')")

        assertEquals("print('x')", executor.lastCode)
        assertEquals("out:print('x')", result.stdout)
    }

    @Test
    fun metricsDelegatesToCodeExecutor() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor)

        val result = controller.metrics()

        assertEquals(11, result.boxesCreatedTotal)
        assertEquals(6, result.totalCommandsExecuted)
    }

    @Test
    fun statefulExecutionUsesConfiguredDefaultLeaseWhenMissing() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor, defaultLeaseSeconds = 45)

        controller.pythonExecuteStateful(
            name = "box-1",
            code = "print('hello')",
            leaseSeconds = null,
        )

        assertEquals(45, executor.lastStatefulRequest?.leaseSeconds)
    }

    @Test
    fun fetchBlobReturnsImageContentForImageFile() {
        val executor = FakeCodeExecutor()
        val blobBytes = byteArrayOf(1, 2, 3)
        executor.fetchBlobResponse = FetchedBlob(
            path = "/workspace/plot.png",
            bytes = blobBytes,
        )
        val controller = McpController(executor)

        val result = controller.fetchBlob(
            path = "/workspace/plot.png",
            name = "box-1",
        )

        assertEquals(
            FetchBlobRequest(
                name = "box-1",
                path = "/workspace/plot.png",
                leaseSeconds = 30,
            ),
            executor.lastFetchBlobRequest,
        )
        assertTrue(result.isError() != true)
        val content = result.content().single() as McpSchema.ImageContent
        assertEquals(Base64.getEncoder().encodeToString(blobBytes), content.data())
        assertEquals("image/png", content.mimeType())
    }

    @Test
    fun fetchBlobReturnsTextContentForNonImageFile() {
        val executor = FakeCodeExecutor()
        val blobBytes = byteArrayOf(9, 8, 7)
        executor.fetchBlobResponse = FetchedBlob(
            path = "/workspace/data.bin",
            bytes = blobBytes,
        )
        val controller = McpController(executor)

        val result = controller.fetchBlob(
            path = "/workspace/data.bin",
            name = "box-1",
        )

        assertTrue(result.isError() != true)
        val content = result.content()
        assertEquals(2, content.size)
        val mimeLine = (content[0] as McpSchema.TextContent).text()
        assertTrue(mimeLine.startsWith("mimeType="))
        assertTrue(!mimeLine.startsWith("mimeType=image/"))
        assertEquals(Base64.getEncoder().encodeToString(blobBytes), (content[1] as McpSchema.TextContent).text())
    }

    @Test
    fun fetchBlobReturnsErrorResultWhenFetchFails() {
        val executor = FakeCodeExecutor()
        executor.fetchBlobFailure = CodeExecutionException("boom")
        val controller = McpController(executor)

        val result = controller.fetchBlob(
            path = "/workspace/data.bin",
            name = "box-1",
        )

        assertEquals(true, result.isError())
        val content = result.content().single() as McpSchema.TextContent
        assertTrue(content.text().contains("Failed to fetch blob from box 'box-1'"))
    }

    @Test
    fun fetchBlobRejectsTmpPathEarly() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor)

        val result = controller.fetchBlob(
            path = "/tmp/restart_test.svg",
            name = "box-1",
        )

        assertEquals(true, result.isError())
        val content = result.content().single() as McpSchema.TextContent
        assertTrue(content.text().contains("/tmp"))
        assertNull(executor.lastFetchBlobRequest)
    }

    private class FakeCodeExecutor : CodeExecutor {
        var lastCode: String? = null
        var lastStatefulRequest: ExecuteStatefulRequest? = null
        var lastFetchBlobRequest: FetchBlobRequest? = null
        var fetchBlobResponse: FetchedBlob = FetchedBlob(
            path = "/workspace/default.bin",
            bytes = byteArrayOf(0),
        )
        var fetchBlobFailure: RuntimeException? = null

        override fun execute(code: String): ExecResult {
            lastCode = code
            return ExecResult(
                exitCode = 0,
                stdout = "out:$code",
                stderr = "",
                errorMessage = null,
                success = true,
            )
        }

        override fun executeStateful(request: ExecuteStatefulRequest): ExecuteStatefulResult {
            lastStatefulRequest = request
            return ExecuteStatefulResult(
                boxId = request.name ?: "auto-box",
                output = ExecResult(
                    exitCode = 0,
                    stdout = "out:${request.code}",
                    stderr = "",
                    errorMessage = null,
                    success = true,
                ),
            )
        }

        override fun fetchBlob(request: FetchBlobRequest): FetchedBlob {
            lastFetchBlobRequest = request
            val failure = fetchBlobFailure
            if (failure != null) {
                throw failure
            }
            return fetchBlobResponse
        }

        override fun metrics(): RuntimeMetricsView {
            return RuntimeMetricsView(
                boxesCreatedTotal = 11,
                boxesFailedTotal = 1,
                boxesStoppedTotal = 4,
                numRunningBoxes = 3,
                totalCommandsExecuted = 6,
                totalExecErrors = 0,
            )
        }
    }
}
