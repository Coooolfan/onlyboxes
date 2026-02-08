package com.coooolfan.onlyboxes.core.model

data class ListActiveContainersRequest(
    val ownerToken: String,
)

data class ActiveContainerView(
    val boxId: String,
    val name: String,
    val remainingDestroySeconds: Long,
)
