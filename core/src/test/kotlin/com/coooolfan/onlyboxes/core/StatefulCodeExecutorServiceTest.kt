package com.coooolfan.onlyboxes.core

import com.coooolfan.onlyboxes.core.exception.BoxExpiredException
import com.coooolfan.onlyboxes.core.exception.BoxNotFoundException
import com.coooolfan.onlyboxes.core.exception.InvalidLeaseException
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.port.BoxFactory
import com.coooolfan.onlyboxes.core.port.BoxSession
import com.coooolfan.onlyboxes.core.service.StatefulCodeExecutorService
import java.time.Clock
import java.time.Instant
import java.time.ZoneId
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertTrue

class StatefulCodeExecutorServiceTest {
    @Test
    fun statelessExecutionCreatesAndClosesTemporaryBox() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        val output = service.execute("print('hello')")

        assertEquals("stdout:print('hello')", output.stdout)
        assertEquals(1, factory.sessions.size)
        assertTrue(factory.sessions.first().closed)
    }

    @Test
    fun statefulExecutionCreatesBoxWhenNameMissing() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        val created = service.executeStateful(
            ExecuteStatefulRequest(
                name = null,
                code = "x = 1",
                leaseSeconds = null,
            ),
        )

        assertTrue(created.boxId.startsWith("auto-session-"))
        assertEquals("stdout:x = 1", created.output.stdout)
    }

    @Test
    fun missingLeaseUsesDefaultThirtySeconds() {
        val clock = MutableClock(Instant.ofEpochMilli(1_000L))
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)

        val created = service.executeStateful(
            ExecuteStatefulRequest(
                name = null,
                code = "print('init')",
                leaseSeconds = null,
            ),
        )

        clock.advanceMillis(30_500L)

        assertFailsWith<BoxExpiredException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    name = created.boxId,
                    code = "print('still alive')",
                    leaseSeconds = null,
                ),
            )
        }
    }

    @Test
    fun nonPositiveLeaseIsRejected() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        assertFailsWith<InvalidLeaseException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    name = null,
                    code = "print('x')",
                    leaseSeconds = 0,
                ),
            )
        }
    }

    @Test
    fun expiredLeaseThrowsAndClosesBox() {
        val clock = MutableClock(Instant.ofEpochMilli(1_000L))
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)

        val created = service.executeStateful(
            ExecuteStatefulRequest(
                name = null,
                code = "print('init')",
                leaseSeconds = 2,
            ),
        )

        clock.advanceMillis(2_500L)

        assertFailsWith<BoxExpiredException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    name = created.boxId,
                    code = "print('still alive')",
                    leaseSeconds = null,
                ),
            )
        }

        assertTrue(factory.sessions.first().closed)
    }

    @Test
    fun unknownNameThrowsNotFound() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        assertFailsWith<BoxNotFoundException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    name = "missing",
                    code = "print('x')",
                    leaseSeconds = null,
                ),
            )
        }
    }

    private class FakeBoxFactory : BoxFactory {
        val sessions = mutableListOf<FakeBoxSession>()

        override fun createStartedBox(): BoxSession {
            val session = FakeBoxSession("session-${sessions.size + 1}")
            sessions += session
            return session
        }

        override fun metrics(): RuntimeMetricsView {
            return RuntimeMetricsView(
                boxesCreatedTotal = sessions.size.toLong(),
                boxesFailedTotal = 0,
                boxesStoppedTotal = sessions.count { it.closed }.toLong(),
                numRunningBoxes = sessions.count { !it.closed }.toLong(),
                totalCommandsExecuted = sessions.sumOf { it.executedCodes.size }.toLong(),
                totalExecErrors = 0,
            )
        }
    }

    private class FakeBoxSession(
        override val sessionId: String,
    ) : BoxSession {
        var closed = false
        val executedCodes = mutableListOf<String>()

        override fun run(code: String): ExecResult {
            executedCodes += code
            return ExecResult(
                exitCode = 0,
                stdout = "stdout:$code",
                stderr = "",
                errorMessage = null,
                success = true,
            )
        }

        override fun close() {
            closed = true
        }
    }

    private class MutableClock(
        private var now: Instant,
    ) : Clock() {
        override fun withZone(zone: ZoneId?): Clock = this

        override fun getZone(): ZoneId = ZoneId.of("UTC")

        override fun instant(): Instant = now

        fun advanceMillis(delta: Long) {
            now = now.plusMillis(delta)
        }
    }
}
