package com.coooolfan.onlyboxes.infra.boxlite

import com.coooolfan.onlyboxes.core.exception.CodeExecutionException
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.port.BoxFactory
import com.coooolfan.onlyboxes.core.port.BoxSession
import io.boxlite.BoxliteRuntime
import io.boxlite.highlevel.CodeBox

class BoxliteBoxFactory : BoxFactory {
    override fun createStartedBox(): BoxSession {
        return try {
            BoxliteCodeSession(CodeBox().start())
        } catch (ex: Exception) {
            throw CodeExecutionException("Failed to create and start CodeBox", ex)
        }
    }

    override fun metrics(): RuntimeMetricsView {
        return try {
            val metrics = BoxliteRuntime.defaultRuntime().metrics().join()
            RuntimeMetricsView(
                boxesCreatedTotal = metrics.boxesCreatedTotal(),
                boxesFailedTotal = metrics.boxesFailedTotal(),
                boxesStoppedTotal = metrics.boxesStoppedTotal(),
                numRunningBoxes = metrics.numRunningBoxes(),
                totalCommandsExecuted = metrics.totalCommandsExecuted(),
                totalExecErrors = metrics.totalExecErrors(),
            )
        } catch (ex: Exception) {
            throw CodeExecutionException("Failed to fetch runtime metrics", ex)
        }
    }
}

private class BoxliteCodeSession(
    private val codeBox: CodeBox,
) : BoxSession {
    override val sessionId: String = codeBox.id()

    override fun run(code: String): ExecResult {
        return try {
            val output = codeBox.run(code)
            ExecResult(
                exitCode = output.exitCode(),
                stdout = output.stdout(),
                stderr = output.stderr(),
                errorMessage = output.errorMessage(),
                success = output.success(),
            )
        } catch (ex: Exception) {
            throw CodeExecutionException("Failed to execute code in box $sessionId", ex)
        }
    }

    override fun close() {
        codeBox.close()
    }
}
