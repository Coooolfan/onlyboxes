package com.coooolfan.onlyboxes.core.model

data class RuntimeMetricsView(
    val boxesCreatedTotal: Long,
    val boxesFailedTotal: Long,
    val boxesStoppedTotal: Long,
    val numRunningBoxes: Long,
    val totalCommandsExecuted: Long,
    val totalExecErrors: Long,
)
