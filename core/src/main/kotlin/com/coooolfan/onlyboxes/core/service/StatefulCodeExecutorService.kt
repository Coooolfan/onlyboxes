package com.coooolfan.onlyboxes.core.service

import com.coooolfan.onlyboxes.core.exception.BoxExpiredException
import com.coooolfan.onlyboxes.core.exception.BoxNotFoundException
import com.coooolfan.onlyboxes.core.exception.CodeExecutionException
import com.coooolfan.onlyboxes.core.exception.InvalidLeaseException
import com.coooolfan.onlyboxes.core.model.ExecResult
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulRequest
import com.coooolfan.onlyboxes.core.model.ExecuteStatefulResult
import com.coooolfan.onlyboxes.core.model.FetchBlobRequest
import com.coooolfan.onlyboxes.core.model.FetchedBlob
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
    private val defaultLeaseSeconds: Long = DEFAULT_LEASE_SECONDS,
) : CodeExecutor {

    init {
        require(defaultLeaseSeconds > 0L) { "defaultLeaseSeconds must be > 0, but was $defaultLeaseSeconds" }
    }

    companion object {
        private const val DEFAULT_LEASE_SECONDS = 30L
    }

    private val boxLeases = ConcurrentHashMap<String, BoxLease>()

    private data class BoxLease(
        val box: BoxSession,
        @Volatile var expiresAtEpochMs: Long?,
    )

    override fun execute(code: String): ExecResult {
        val box = boxFactory.createStartedBox()
        return box.use { it.run(code) }
    }

    override fun executeStateful(request: ExecuteStatefulRequest): ExecuteStatefulResult {
        val leaseSeconds = request.leaseSeconds ?: defaultLeaseSeconds
        val normalizedName = request.name?.trim()?.takeIf { it.isNotEmpty() }
        if (normalizedName == null) {
            val box = boxFactory.createStartedBox()
            val lease = BoxLease(box = box, expiresAtEpochMs = null)
            renewLease(lease, leaseSeconds)

            val newName = "auto-${box.sessionId}"
            boxLeases[newName] = lease
            return ExecuteStatefulResult(boxId = newName, output = box.run(request.code))
        }

        val lease = boxLeases[normalizedName] ?: throw BoxNotFoundException(normalizedName)
        cleanupIfExpired(normalizedName, lease)
        renewLease(lease, leaseSeconds)
        return ExecuteStatefulResult(boxId = normalizedName, output = lease.box.run(request.code))
    }

    override fun fetchBlob(request: FetchBlobRequest): FetchedBlob {
        val normalizedName = request.name.trim()
            .takeIf { it.isNotEmpty() }
            ?: throw CodeExecutionException("Box name must not be empty")
        val normalizedPath = request.path.trim()
        if (normalizedPath.isEmpty()) {
            throw CodeExecutionException("Blob file path must not be empty")
        }

        val leaseSeconds = request.leaseSeconds ?: defaultLeaseSeconds
        val lease = boxLeases[normalizedName] ?: throw BoxNotFoundException(normalizedName)
        cleanupIfExpired(normalizedName, lease)
        renewLease(lease, leaseSeconds)

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

    override fun metrics(): RuntimeMetricsView = boxFactory.metrics()

    private fun renewLease(lease: BoxLease, leaseSeconds: Long) {
        if (leaseSeconds <= 0L) {
            throw InvalidLeaseException(leaseSeconds)
        }

        val deltaMillis = try {
            Math.multiplyExact(leaseSeconds, 1000L)
        } catch (_: ArithmeticException) {
            throw InvalidLeaseException(leaseSeconds)
        }

        lease.expiresAtEpochMs = clock.millis() + deltaMillis
    }

    private fun cleanupIfExpired(name: String, lease: BoxLease) {
        val expiresAt = lease.expiresAtEpochMs ?: return
        if (clock.millis() < expiresAt) {
            return
        }

        boxLeases.remove(name, lease)
        closeQuietly(lease.box)
        throw BoxExpiredException(name)
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
