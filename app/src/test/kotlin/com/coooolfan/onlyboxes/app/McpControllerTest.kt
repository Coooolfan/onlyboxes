package com.coooolfan.onlyboxes.app

import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulResult
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.service.CodeExecutor
import kotlin.test.Test
import kotlin.test.assertEquals

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

    private class FakeCodeExecutor : CodeExecutor {
        var lastCode: String? = null
        var lastStatefulRequest: ExecuteStatefulRequest? = null

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
