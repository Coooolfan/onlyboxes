package com.coooolfan.onlyboxes.app

import com.coooolfan.onlyboxes.core.exception.CodeExecutionException
import com.coooolfan.onlyboxes.core.model.ActiveContainerView
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulResult
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.FetchedBlob
import com.coooolfan.onlyboxes.core.model.ListActiveContainersRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.service.CodeExecutor
import io.modelcontextprotocol.spec.McpSchema
import java.util.Base64
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNull
import kotlin.test.assertTrue

class McpControllerTest {
    @Test
    fun statefulExecutionDelegatesToCodeExecutor() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

        val result = controller.pythonExecuteStateful(
            name = "box-1",
            code = "print('hello')",
            leaseSeconds = 20,
        )

        assertEquals("box-1", result.boxId)
        assertEquals(false, result.destroyed)
        assertEquals(180, result.remainingDestroySeconds)
        assertEquals("token-a", executor.lastStatefulRequest?.ownerToken)
        assertEquals("print('hello')", executor.lastStatefulRequest?.code)
        assertEquals(20, executor.lastStatefulRequest?.leaseSeconds)
        assertEquals("out:print('hello')", result.output.stdout)
    }

    @Test
    fun statelessExecutionDelegatesToCodeExecutor() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

        val result = controller.pythonExecute("print('x')")

        assertEquals("print('x')", executor.lastCode)
        assertEquals("out:print('x')", result.stdout)
    }

    @Test
    fun metricsDelegatesToCodeExecutor() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

        val result = controller.metrics()

        assertEquals(11, result.boxesCreatedTotal)
        assertEquals(6, result.totalCommandsExecuted)
    }

    @Test
    fun statefulExecutionPassesLeaseSeconds() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

        controller.pythonExecuteStateful(
            name = "box-1",
            code = "print('hello')",
            leaseSeconds = 45,
        )

        assertEquals(45, executor.lastStatefulRequest?.leaseSeconds)
    }

    @Test
    fun listActiveContainersDelegatesToCodeExecutor() {
        val executor = FakeCodeExecutor().apply {
            listActiveContainersResponse = listOf(
                ActiveContainerView(
                    boxId = "auto-session-2",
                    name = "auto-session-2",
                    remainingDestroySeconds = 90,
                ),
                ActiveContainerView(
                    boxId = "auto-session-7",
                    name = "auto-session-7",
                    remainingDestroySeconds = 12,
                ),
            )
        }
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

        val result = controller.listActiveContainers()

        assertEquals(
            ListActiveContainersRequest(ownerToken = "token-a"),
            executor.lastListActiveContainersRequest,
        )
        assertEquals(executor.listActiveContainersResponse, result)
    }

    @Test
    fun fetchBlobReturnsImageContentForImageFile() {
        val executor = FakeCodeExecutor()
        val blobBytes = byteArrayOf(1, 2, 3)
        executor.fetchBlobResponse = FetchedBlob(
            path = "/workspace/plot.png",
            bytes = blobBytes,
        )
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

        val result = controller.fetchBlob(
            path = "/workspace/plot.png",
            name = "box-1",
        )

        assertEquals(
            FetchBlobRequest(
                ownerToken = "token-a",
                name = "box-1",
                path = "/workspace/plot.png",
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
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

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
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

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
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

        val result = controller.fetchBlob(
            path = "/tmp/restart_test.svg",
            name = "box-1",
        )

        assertEquals(true, result.isError())
        val content = result.content().single() as McpSchema.TextContent
        assertTrue(content.text().contains("/tmp"))
        assertNull(executor.lastFetchBlobRequest)
    }

    @Test
    fun statefulExecutionFailsWhenTokenContextMissing() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor, FixedAuthTokenProvider(null))

        assertFailsWith<CodeExecutionException> {
            controller.pythonExecuteStateful(
                name = "box-1",
                code = "print('hello')",
                leaseSeconds = 20,
            )
        }
    }

    @Test
    fun listActiveContainersFailsWhenTokenContextMissing() {
        val executor = FakeCodeExecutor()
        val controller = McpController(executor, FixedAuthTokenProvider(null))

        assertFailsWith<CodeExecutionException> {
            controller.listActiveContainers()
        }
    }

    @Test
    fun statefulExecutionReturnsDestroyedWhenExecutorDestroysContainer() {
        val executor = FakeCodeExecutor().apply {
            nextStatefulResult = ExecuteStatefulResult(
                boxId = null,
                destroyed = true,
                remainingDestroySeconds = 0,
                output = ExecResult(
                    exitCode = 0,
                    stdout = "out:print('bye')",
                    stderr = "",
                    errorMessage = null,
                    success = true,
                ),
            )
        }
        val controller = McpController(executor, FixedAuthTokenProvider("token-a"))

        val result = controller.pythonExecuteStateful(
            name = "box-1",
            code = "print('bye')",
            leaseSeconds = -1,
        )

        assertEquals(null, result.boxId)
        assertEquals(true, result.destroyed)
        assertEquals(0, result.remainingDestroySeconds)
        assertEquals(-1, executor.lastStatefulRequest?.leaseSeconds)
    }

    private class FakeCodeExecutor : CodeExecutor {
        var lastCode: String? = null
        var lastStatefulRequest: ExecuteStatefulRequest? = null
        var lastFetchBlobRequest: FetchBlobRequest? = null
        var lastListActiveContainersRequest: ListActiveContainersRequest? = null
        var fetchBlobResponse: FetchedBlob = FetchedBlob(
            path = "/workspace/default.bin",
            bytes = byteArrayOf(0),
        )
        var fetchBlobFailure: RuntimeException? = null
        var nextStatefulResult: ExecuteStatefulResult? = null
        var listActiveContainersResponse: List<ActiveContainerView> = emptyList()

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
            val presetResult = nextStatefulResult
            if (presetResult != null) {
                return presetResult
            }
            return ExecuteStatefulResult(
                boxId = request.name ?: "auto-box",
                destroyed = false,
                remainingDestroySeconds = 180,
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

        override fun listActiveContainers(request: ListActiveContainersRequest): List<ActiveContainerView> {
            lastListActiveContainersRequest = request
            return listActiveContainersResponse
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

    private class FixedAuthTokenProvider(
        private val token: String?,
    ) : AuthTokenProvider {
        override fun requireToken(): String {
            return token ?: throw CodeExecutionException("Authentication token context is missing")
        }
    }
}
