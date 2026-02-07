package com.coooolfan.onlyboxes.core.port

import com.coooolfan.onlyboxes.core.model.RuntimeMetricsView

interface BoxFactory {
    fun createStartedBox(): BoxSession

    fun metrics(): RuntimeMetricsView
}
