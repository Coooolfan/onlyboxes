package com.coooolfan.onlyboxes.core.service

import com.coooolfan.onlyboxes.core.exception.BoxExpiredException
import com.coooolfan.onlyboxes.core.exception.BoxNotFoundException
import com.coooolfan.onlyboxes.core.exception.CodeExecutionException
import com.coooolfan.onlyboxes.core.model.ActiveContainerView
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulResult
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.FetchedBlob
import com.coooolfan.onlyboxes.core.model.ListActiveContainersRequest
import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView
import com.coooolfan.onlyboxes.core.port.BoxFactory
import com.coooolfan.onlyboxes.core.port.BoxSession
import java.nio.file.Files
import java.nio.file.Path
import java.time.Clock
import java.util.concurrent.ConcurrentHashMap

class StatefulCodeExecutorService(
    private val boxFactory: BoxFactory,
    private val clock: Clock = Clock.systemUTC(),
    private val minLeaseSeconds: Long = DEFAULT_MIN_LEASE_SECONDS,
    private val maxLeaseSeconds: Long = DEFAULT_MAX_LEASE_SECONDS,
) : CodeExecutor {

    init {
        require(minLeaseSeconds > 0L) { "minLeaseSeconds must be > 0, but was $minLeaseSeconds" }
        require(maxLeaseSeconds > 0L) { "maxLeaseSeconds must be > 0, but was $maxLeaseSeconds" }
        if (minLeaseSeconds > maxLeaseSeconds) {
            throw IllegalStateException(
                "onlyboxes.lease.min-seconds must be <= onlyboxes.lease.max-seconds, " +
                    "but was min=$minLeaseSeconds max=$maxLeaseSeconds",
            )
        }
    }

    companion object {
        private const val DEFAULT_MIN_LEASE_SECONDS = 30L
        private const val DEFAULT_MAX_LEASE_SECONDS = 3600L
    }

    private val boxLeases = ConcurrentHashMap<LeaseKey, BoxLease>()

    private data class LeaseKey(
        val ownerToken: String,
        val boxName: String,
    )

    private data class BoxLease(
        val box: BoxSession,
        @Volatile var expiresAtEpochMs: Long?,
    )

    override fun execute(code: String): ExecResult {
        val box = boxFactory.createStartedBox()
        return box.use { it.run(code) }
    }

    override fun executeStateful(request: ExecuteStatefulRequest): ExecuteStatefulResult {
        val ownerToken = normalizeOwnerToken(request.ownerToken)
        val normalizedName = request.name?.trim()?.takeIf { it.isNotEmpty() }
        if (normalizedName == null) {
            return createStateful(ownerToken, request.code, request.leaseSeconds)
        }
        return continueStateful(ownerToken, normalizedName, request.code, request.leaseSeconds)
    }

    override fun fetchBlob(request: FetchBlobRequest): FetchedBlob {
        val ownerToken = normalizeOwnerToken(request.ownerToken)
        val normalizedName = request.name.trim()
            .takeIf { it.isNotEmpty() }
            ?: throw CodeExecutionException("Box name must not be empty")
        val normalizedPath = request.path.trim()
        if (normalizedPath.isEmpty()) {
            throw CodeExecutionException("Blob file path must not be empty")
        }

        val leaseKey = LeaseKey(ownerToken = ownerToken, boxName = normalizedName)
        val lease = boxLeases[leaseKey] ?: throw BoxNotFoundException(normalizedName)
        cleanupIfExpired(leaseKey, normalizedName, lease)

        val tempDir = try {
            Files.createTempDirectory("onlyboxes-copyout-")
        } catch (ex: Exception) {
            throw CodeExecutionException("Failed to create temporary directory for copyOut", ex)
        }

        try {
            lease.box.copyOut(normalizedPath, tempDir)
            val copiedFiles = copiedRegularFiles(tempDir)
            return when (copiedFiles.size) {
                0 -> throw CodeExecutionException("No file copied from path '$normalizedPath'")
                1 -> FetchedBlob(
                    path = normalizedPath,
                    bytes = Files.readAllBytes(copiedFiles.first()),
                )
                else -> throw CodeExecutionException("Path resolves to multiple files; file path required")
            }
        } catch (ex: CodeExecutionException) {
            throw ex
        } catch (ex: Exception) {
            val reason = ex.message ?: ex.javaClass.simpleName
            throw CodeExecutionException("Failed to fetch blob from box '$normalizedName': $reason", ex)
        } finally {
            deleteRecursivelyQuietly(tempDir)
        }
    }

    override fun listActiveContainers(request: ListActiveContainersRequest): List<ActiveContainerView> {
        val ownerToken = normalizeOwnerToken(request.ownerToken)
        val now = clock.millis()
        val activeContainers = mutableListOf<ActiveContainerView>()

        boxLeases.forEach { (leaseKey, lease) ->
            if (leaseKey.ownerToken != ownerToken) {
                return@forEach
            }

            val expiresAt = lease.expiresAtEpochMs
            if (expiresAt == null || now >= expiresAt) {
                boxLeases.remove(leaseKey, lease)
                closeQuietly(lease.box)
                return@forEach
            }

            activeContainers += ActiveContainerView(
                boxId = leaseKey.boxName,
                name = leaseKey.boxName,
                remainingDestroySeconds = computeRemainingSecondsCeil(expiresAt, now),
            )
        }

        return activeContainers.sortedBy { it.boxId }
    }

    override fun metrics(): RuntimeMetricsView = boxFactory.metrics()

    private fun createStateful(
        ownerToken: String,
        code: String,
        leaseSeconds: Long,
    ): ExecuteStatefulResult {
        val box = boxFactory.createStartedBox()
        val output = try {
            box.run(code)
        } catch (ex: Exception) {
            closeQuietly(box)
            throw ex
        }

        if (leaseSeconds < 0L) {
            closeQuietly(box)
            return ExecuteStatefulResult(
                boxId = null,
                destroyed = true,
                remainingDestroySeconds = 0L,
                output = output,
            )
        }

        val appliedLeaseSeconds = clampLeaseSeconds(leaseSeconds)
        val candidateExpiresAt = candidateExpiresAtEpochMs(appliedLeaseSeconds)
        val newName = "auto-${box.sessionId}"
        boxLeases[LeaseKey(ownerToken = ownerToken, boxName = newName)] = BoxLease(
            box = box,
            expiresAtEpochMs = candidateExpiresAt,
        )

        return ExecuteStatefulResult(
            boxId = newName,
            destroyed = false,
            remainingDestroySeconds = computeRemainingSecondsCeil(candidateExpiresAt),
            output = output,
        )
    }

    private fun continueStateful(
        ownerToken: String,
        boxName: String,
        code: String,
        leaseSeconds: Long,
    ): ExecuteStatefulResult {
        val leaseKey = LeaseKey(ownerToken = ownerToken, boxName = boxName)
        val lease = boxLeases[leaseKey] ?: throw BoxNotFoundException(boxName)
        cleanupIfExpired(leaseKey, boxName, lease)

        if (leaseSeconds < 0L) {
            val output = try {
                lease.box.run(code)
            } finally {
                destroyLease(leaseKey, lease)
            }
            return ExecuteStatefulResult(
                boxId = null,
                destroyed = true,
                remainingDestroySeconds = 0L,
                output = output,
            )
        }

        val output = lease.box.run(code)
        val appliedLeaseSeconds = clampLeaseSeconds(leaseSeconds)
        val candidateExpiresAt = candidateExpiresAtEpochMs(appliedLeaseSeconds)
        val currentExpiresAt = lease.expiresAtEpochMs ?: Long.MIN_VALUE
        val nextExpiresAt = maxOf(currentExpiresAt, candidateExpiresAt)
        lease.expiresAtEpochMs = nextExpiresAt

        return ExecuteStatefulResult(
            boxId = boxName,
            destroyed = false,
            remainingDestroySeconds = computeRemainingSecondsCeil(nextExpiresAt),
            output = output,
        )
    }

    private fun cleanupIfExpired(leaseKey: LeaseKey, name: String, lease: BoxLease) {
        val expiresAt = lease.expiresAtEpochMs ?: return
        if (clock.millis() < expiresAt) {
            return
        }

        boxLeases.remove(leaseKey, lease)
        closeQuietly(lease.box)
        throw BoxExpiredException(name)
    }

    private fun destroyLease(leaseKey: LeaseKey, lease: BoxLease) {
        boxLeases.remove(leaseKey, lease)
        closeQuietly(lease.box)
    }

    private fun clampLeaseSeconds(requestedLeaseSeconds: Long): Long {
        return requestedLeaseSeconds.coerceIn(minLeaseSeconds, maxLeaseSeconds)
    }

    private fun candidateExpiresAtEpochMs(appliedLeaseSeconds: Long): Long {
        val deltaMillis = Math.multiplyExact(appliedLeaseSeconds, 1000L)
        return Math.addExact(clock.millis(), deltaMillis)
    }

    private fun computeRemainingSecondsCeil(expiresAtEpochMs: Long): Long {
        return computeRemainingSecondsCeil(expiresAtEpochMs, clock.millis())
    }

    private fun computeRemainingSecondsCeil(expiresAtEpochMs: Long, nowEpochMs: Long): Long {
        val remainingMillis = expiresAtEpochMs - nowEpochMs
        if (remainingMillis <= 0L) {
            return 0L
        }
        return (remainingMillis + 999L) / 1000L
    }

    private fun normalizeOwnerToken(ownerTokenRaw: String): String {
        return ownerTokenRaw.trim()
            .takeIf { it.isNotEmpty() }
            ?: throw CodeExecutionException("Owner token must not be empty")
    }

    fun closeAll() {
        boxLeases.values.forEach { closeQuietly(it.box) }
        boxLeases.clear()
    }

    private fun copiedRegularFiles(root: Path): List<Path> {
        return Files.walk(root).use { walk ->
            walk.filter { Files.isRegularFile(it) }.toList()
        }
    }

    private fun deleteRecursivelyQuietly(root: Path) {
        try {
            Files.walk(root).use { walk ->
                walk
                    .sorted { left, right -> right.compareTo(left) }
                    .forEach { path ->
                        try {
                            Files.deleteIfExists(path)
                        } catch (_: Exception) {
                            // Temporary files may already be cleaned up by the OS.
                        }
                    }
            }
        } catch (_: Exception) {
            // Best-effort cleanup only.
        }
    }

    private fun closeQuietly(box: BoxSession) {
        try {
            box.close()
        } catch (_: Exception) {
            // Box may already be gone. We intentionally ignore close failures.
        }
    }
}
