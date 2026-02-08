package com.coooolfan.onlyboxes.core

import com.coooolfan.onlyboxes.core.exception.BoxExpiredException
import com.coooolfan.onlyboxes.core.exception.BoxNotFoundException
import com.coooolfan.onlyboxes.core.exception.CodeExecutionException
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.ListActiveContainersRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.port.BoxFactory
import com.coooolfan.onlyboxes.core.port.BoxSession
import com.coooolfan.onlyboxes.core.service.StatefulCodeExecutorService
import java.nio.file.Files
import java.nio.file.Path
import java.time.Clock
import java.time.Instant
import java.time.ZoneId
import kotlin.test.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNotNull
import kotlin.test.assertNull
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
                ownerToken = "token-a",
                name = null,
                code = "x = 1",
                leaseSeconds = 30,
            ),
        )

        assertNotNull(created.boxId)
        assertEquals(false, created.destroyed)
        assertEquals(30, created.remainingDestroySeconds)
        assertEquals("stdout:x = 1", created.output.stdout)
    }

    @Test
    fun leaseSecondsLowerBoundIsApplied() {
        val clock = MutableClock(Instant.ofEpochMilli(1_000L))
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)

        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 10,
            ),
        )

        assertEquals(30, created.remainingDestroySeconds)
    }

    @Test
    fun leaseSecondsUpperBoundIsApplied() {
        val clock = MutableClock(Instant.ofEpochMilli(1_000L))
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)

        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 99_999,
            ),
        )

        assertEquals(3600, created.remainingDestroySeconds)
    }

    @Test
    fun leaseStartsAfterCommandExecutionAndShorterRenewDoesNotReduceExpiry() {
        val clock = MutableClock(Instant.ofEpochMilli(0L))
        val factory = FakeBoxFactory()
        factory.nextSessionRunHook = { clock.advanceMillis(600_000L) } // 10 min
        val service = StatefulCodeExecutorService(factory, clock)

        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('first')",
                leaseSeconds = 3600,
            ),
        )
        val boxId = assertNotNull(created.boxId)
        assertEquals(3600, created.remainingDestroySeconds)

        // Move from 12:10 to 12:30.
        clock.advanceMillis(1_200_000L)
        factory.sessions.first().runHook = { clock.advanceMillis(600_000L) } // 10 min

        val renewed = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = boxId,
                code = "print('second')",
                leaseSeconds = 10,
            ),
        )

        // Must remain 13:10, so at 12:40 there are 1800s left.
        assertEquals(false, renewed.destroyed)
        assertEquals(boxId, renewed.boxId)
        assertEquals(1800, renewed.remainingDestroySeconds)
    }

    @Test
    fun remainingDestroySecondsUsesCeiling() {
        val clock = MutableClock(
            now = Instant.ofEpochMilli(1_000L),
            stepMillisOnRead = 1L,
        )
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)

        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )

        // If floor was used this could become 29 because clock moves between now() calls.
        assertEquals(30, created.remainingDestroySeconds)
    }

    @Test
    fun negativeLeaseOnCreateDestroysImmediately() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = -1,
            ),
        )

        assertEquals(true, created.destroyed)
        assertNull(created.boxId)
        assertEquals(0, created.remainingDestroySeconds)
        assertTrue(factory.sessions.first().closed)
        assertFailsWith<BoxNotFoundException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    ownerToken = "token-a",
                    name = "auto-session-1",
                    code = "print('x')",
                    leaseSeconds = 30,
                ),
            )
        }
    }

    @Test
    fun negativeLeaseOnExistingDestroysImmediately() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)

        val destroyed = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = boxId,
                code = "print('destroy')",
                leaseSeconds = -1,
            ),
        )

        assertEquals(true, destroyed.destroyed)
        assertNull(destroyed.boxId)
        assertEquals(0, destroyed.remainingDestroySeconds)
        assertTrue(factory.sessions.first().closed)
        assertFailsWith<BoxNotFoundException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    ownerToken = "token-a",
                    name = boxId,
                    code = "print('again')",
                    leaseSeconds = 30,
                ),
            )
        }
    }

    @Test
    fun invalidLeaseBoundsThrowOnStartup() {
        val factory = FakeBoxFactory()

        assertFailsWith<IllegalStateException> {
            StatefulCodeExecutorService(
                boxFactory = factory,
                minLeaseSeconds = 60,
                maxLeaseSeconds = 30,
            )
        }
    }

    @Test
    fun unknownNameThrowsNotFound() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        assertFailsWith<BoxNotFoundException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    ownerToken = "token-a",
                    name = "missing",
                    code = "print('x')",
                    leaseSeconds = 30,
                ),
            )
        }
    }

    @Test
    fun crossTokenStatefulAccessThrowsNotFound() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)

        assertFailsWith<BoxNotFoundException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    ownerToken = "token-b",
                    name = boxId,
                    code = "print('x')",
                    leaseSeconds = 30,
                ),
            )
        }
    }

    @Test
    fun sameTokenCanAccessOwnContainer() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)

        val continued = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = boxId,
                code = "print('again')",
                leaseSeconds = 30,
            ),
        )

        assertEquals(false, continued.destroyed)
        assertEquals(boxId, continued.boxId)
        assertEquals("stdout:print('again')", continued.output.stdout)
    }

    @Test
    fun crossTokenFetchBlobThrowsNotFound() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)

        assertFailsWith<BoxNotFoundException> {
            service.fetchBlob(
                FetchBlobRequest(
                    ownerToken = "token-b",
                    name = boxId,
                    path = "/workspace/blob.bin",
                ),
            )
        }
    }

    @Test
    fun metricsRemainSharedAcrossTokens() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('a')",
                leaseSeconds = 30,
            ),
        )
        service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-b",
                name = null,
                code = "print('b')",
                leaseSeconds = 30,
            ),
        )

        val metrics = service.metrics()
        assertEquals(2, metrics.boxesCreatedTotal)
        assertEquals(2, metrics.totalCommandsExecuted)
    }

    @Test
    fun listActiveContainersReturnsCurrentTokenOnlyAndIncludesBothNames() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)
        val first = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('a-1')",
                leaseSeconds = 30,
            ),
        )
        val second = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('a-2')",
                leaseSeconds = 30,
            ),
        )
        service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-b",
                name = null,
                code = "print('b-1')",
                leaseSeconds = 30,
            ),
        )

        val tokenA = service.listActiveContainers(
            ListActiveContainersRequest(ownerToken = "token-a"),
        )

        val expectedTokenAIds = setOf(assertNotNull(first.boxId), assertNotNull(second.boxId))
        assertEquals(expectedTokenAIds, tokenA.map { it.boxId }.toSet())
        assertEquals(tokenA.map { it.boxId }.sorted(), tokenA.map { it.boxId })
        tokenA.forEach { container ->
            assertEquals(container.boxId, container.name)
            assertTrue(container.remainingDestroySeconds >= 1)
        }

        val tokenB = service.listActiveContainers(
            ListActiveContainersRequest(ownerToken = "token-b"),
        )
        assertEquals(1, tokenB.size)
        assertEquals(tokenB.single().boxId, tokenB.single().name)
    }

    @Test
    fun listActiveContainersReturnsEmptyWhenNoActiveContainers() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        val listed = service.listActiveContainers(
            ListActiveContainersRequest(ownerToken = "token-a"),
        )

        assertEquals(emptyList(), listed)
    }

    @Test
    fun listActiveContainersSkipsExpiredAndCleansThemUp() {
        val clock = MutableClock(Instant.ofEpochMilli(1_000L))
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)
        val expired = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('old')",
                leaseSeconds = 30,
            ),
        )
        val expiredBoxId = assertNotNull(expired.boxId)

        clock.advanceMillis(30_500L)
        val active = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('new')",
                leaseSeconds = 30,
            ),
        )
        val activeBoxId = assertNotNull(active.boxId)

        val listed = service.listActiveContainers(
            ListActiveContainersRequest(ownerToken = "token-a"),
        )

        assertEquals(listOf(activeBoxId), listed.map { it.boxId })
        assertTrue(factory.sessions[0].closed)
        assertTrue(!factory.sessions[1].closed)
        assertFailsWith<BoxNotFoundException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    ownerToken = "token-a",
                    name = expiredBoxId,
                    code = "print('expired')",
                    leaseSeconds = 30,
                ),
            )
        }
    }

    @Test
    fun listActiveContainersDoesNotRenewLease() {
        val clock = MutableClock(Instant.ofEpochMilli(1_000L))
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)

        clock.advanceMillis(29_500L)
        val listed = service.listActiveContainers(
            ListActiveContainersRequest(ownerToken = "token-a"),
        )
        assertEquals(1, listed.size)
        assertEquals(1, listed.single().remainingDestroySeconds)

        clock.advanceMillis(600L)
        assertFailsWith<BoxExpiredException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    ownerToken = "token-a",
                    name = boxId,
                    code = "print('after list')",
                    leaseSeconds = 30,
                ),
            )
        }
    }

    @Test
    fun fetchBlobReadsSingleCopiedFile() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)
        val expected = byteArrayOf(1, 2, 3, 4)
        val session = factory.sessions.first()
        session.copyOutHandler = { _, hostDest ->
            Files.write(hostDest.resolve("plot.png"), expected)
        }

        val blob = service.fetchBlob(
            FetchBlobRequest(
                ownerToken = "token-a",
                name = boxId,
                path = "/workspace/plot.png",
            ),
        )

        assertEquals("/workspace/plot.png", blob.path)
        assertContentEquals(expected, blob.bytes)
        assertEquals(listOf("/workspace/plot.png"), session.copiedSources)
    }

    @Test
    fun fetchBlobRejectsBlankPath() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)

        assertFailsWith<CodeExecutionException> {
            service.fetchBlob(
                FetchBlobRequest(
                    ownerToken = "token-a",
                    name = boxId,
                    path = "   ",
                ),
            )
        }
    }

    @Test
    fun fetchBlobUnknownNameThrowsNotFound() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)

        assertFailsWith<BoxNotFoundException> {
            service.fetchBlob(
                FetchBlobRequest(
                    ownerToken = "token-a",
                    name = "missing",
                    path = "/tmp/blob.bin",
                ),
            )
        }
    }

    @Test
    fun fetchBlobDoesNotRenewLease() {
        val clock = MutableClock(Instant.ofEpochMilli(1_000L))
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)

        clock.advanceMillis(29_500L)
        service.fetchBlob(
            FetchBlobRequest(
                ownerToken = "token-a",
                name = boxId,
                path = "/workspace/blob.bin",
            ),
        )

        clock.advanceMillis(600L)
        assertFailsWith<BoxExpiredException> {
            service.executeStateful(
                ExecuteStatefulRequest(
                    ownerToken = "token-a",
                    name = boxId,
                    code = "print('after fetch')",
                    leaseSeconds = 30,
                ),
            )
        }
    }

    @Test
    fun fetchBlobExpiredLeaseThrowsAndClosesBox() {
        val clock = MutableClock(Instant.ofEpochMilli(1_000L))
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory, clock)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)

        clock.advanceMillis(30_500L)

        assertFailsWith<BoxExpiredException> {
            service.fetchBlob(
                FetchBlobRequest(
                    ownerToken = "token-a",
                    name = boxId,
                    path = "/tmp/blob.bin",
                ),
            )
        }

        assertTrue(factory.sessions.first().closed)
    }

    @Test
    fun fetchBlobFailsWhenCopyOutReturnsMultipleFiles() {
        val factory = FakeBoxFactory()
        val service = StatefulCodeExecutorService(factory)
        val created = service.executeStateful(
            ExecuteStatefulRequest(
                ownerToken = "token-a",
                name = null,
                code = "print('init')",
                leaseSeconds = 30,
            ),
        )
        val boxId = assertNotNull(created.boxId)
        val session = factory.sessions.first()
        session.copyOutHandler = { _, hostDest ->
            Files.write(hostDest.resolve("a.txt"), "a".toByteArray())
            Files.write(hostDest.resolve("b.txt"), "b".toByteArray())
        }

        val ex = assertFailsWith<CodeExecutionException> {
            service.fetchBlob(
                FetchBlobRequest(
                    ownerToken = "token-a",
                    name = boxId,
                    path = "/workspace/multi",
                ),
            )
        }

        assertTrue(ex.message?.contains("multiple files") == true)
    }

    private class FakeBoxFactory : BoxFactory {
        val sessions = mutableListOf<FakeBoxSession>()
        var nextSessionRunHook: ((String) -> Unit)? = null

        override fun createStartedBox(): BoxSession {
            val session = FakeBoxSession("session-${sessions.size + 1}")
            val runHook = nextSessionRunHook
            if (runHook != null) {
                session.runHook = runHook
                nextSessionRunHook = null
            }
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
        val copiedSources = mutableListOf<String>()
        var runHook: (String) -> Unit = {}
        var copyOutHandler: (containerSrc: String, hostDest: Path) -> Unit = { containerSrc, hostDest ->
            val fileName = runCatching { Path.of(containerSrc).fileName?.toString() }
                .getOrNull()
                ?.takeIf { it.isNotBlank() }
                ?: "blob.bin"
            Files.write(hostDest.resolve(fileName), "blob:$containerSrc".toByteArray())
        }

        override fun run(code: String): ExecResult {
            executedCodes += code
            runHook(code)
            return ExecResult(
                exitCode = 0,
                stdout = "stdout:$code",
                stderr = "",
                errorMessage = null,
                success = true,
            )
        }

        override fun copyOut(containerSrc: String, hostDest: Path) {
            copiedSources += containerSrc
            copyOutHandler(containerSrc, hostDest)
        }

        override fun close() {
            closed = true
        }
    }

    private class MutableClock(
        private var now: Instant,
        private val stepMillisOnRead: Long = 0L,
    ) : Clock() {
        override fun withZone(zone: ZoneId?): Clock = this

        override fun getZone(): ZoneId = ZoneId.of("UTC")

        override fun instant(): Instant {
            val current = now
            if (stepMillisOnRead != 0L) {
                now = now.plusMillis(stepMillisOnRead)
            }
            return current
        }

        fun advanceMillis(delta: Long) {
            now = now.plusMillis(delta)
        }
    }
}
