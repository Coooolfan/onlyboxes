package com.coooolfan.onlyboxes.core.exception

open class CodeExecutionException(message: String, cause: Throwable? = null) : RuntimeException(message, cause)

class InvalidLeaseException(leaseSeconds: Long) :
    CodeExecutionException("leaseSeconds must be > 0, but was $leaseSeconds")

class BoxNotFoundException(name: String) :
    CodeExecutionException("Box not found for name $name")

class BoxExpiredException(name: String) :
    CodeExecutionException("Box expired for name $name")
